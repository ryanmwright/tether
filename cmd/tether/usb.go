package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/usbip"
)

func newUSBCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usb",
		Short: "Share USB devices with a host (USB/IP)",
		Long: "Share USB devices plugged in here with a host, over USB/IP through the\n" +
			"host's connection. Name a device by bus ID (1-2.3: whatever is in that\n" +
			"port) or vendor:product (1050:0407: that device, in any port); see\n" +
			"`tether usb list`.\n\n" +
			"While shared, a device is gone from this machine. It comes back when you\n" +
			"detach it, the connection drops, or the daemon exits.\n\n" +
			"Needs the USB/IP helper running as root here (`tether usbip-helper`), and\n" +
			"on the remote: usbip, the vhci-hcd kernel module, and passwordless sudo\n" +
			"for usbip (NixOS: tether.remote.usb.enable).",
	}
	cmd.AddCommand(newUSBListCmd(g), newUSBAttachCmd(g), newUSBDetachCmd(g))
	return cmd
}

func newUSBListCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the USB devices that can be shared, and where they are",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			var st api.Status
			if err := c.Call(cmd.Context(), api.MethodStatus, nil, &st); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if st.USBUnavailable != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "note:", st.USBUnavailable)
			}
			if len(st.USB) == 0 {
				fmt.Fprintln(out, "no USB devices")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "BUS ID\tID\tNAME\tSHARED WITH")
			for _, d := range st.USB {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.BusID, d.ID, d.Name, d.Host)
			}
			return tw.Flush()
		},
	}
}

func newUSBAttachCmd(g *globalFlags) *cobra.Command {
	var noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "attach HOST DEVICE",
		Short: "Share a device with HOST, connecting it if needed",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.connect(ctx)
			if err != nil {
				return err
			}
			defer c.Close()
			var next nextFunc
			if !noWait {
				if next, err = subscribe(ctx, c); err != nil {
					return err
				}
			}
			var res api.USBResult
			if err := c.Call(ctx, api.MethodUSBAttach, api.USBParams{Host: args[0], Device: args[1]}, &res); err != nil {
				return err
			}
			if noWait {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: added\n", res.Device)
				return nil
			}
			wctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			st, err := waitUntil(wctx, next, res.Generation, func(st api.Status) bool {
				return findHost(st, res.Host).State == api.StateError || settled(findUSB(st, res.Host, res.Device).State)
			})
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("still not attached after %s; check `tether status`", timeout)
			}
			if err != nil {
				return err
			}
			if h := findHost(st, res.Host); h.State == api.StateError {
				return fmt.Errorf("%s: %s (the device is kept and will be attached once connected)", res.Host, h.Error)
			}
			if u := findUSB(st, res.Host, res.Device); u.State == api.StateError {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: error: %s\n", res.Device, u.Error)
				fmt.Fprintf(cmd.ErrOrStderr(), "failed devices are kept and retried; remove with `tether usb detach %s %s`\n", args[0], res.Device)
				return errNotHealthy
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: attached to %s\n", res.Device, res.Host)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the result")
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "how long to wait")
	return cmd
}

func newUSBDetachCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "detach HOST DEVICE",
		Short: "Stop sharing a device and give it back to this machine",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			g.noAutostart = true
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.USBResult
			if err := c.Call(cmd.Context(), api.MethodUSBDetach, api.USBParams{Host: args[0], Device: args[1]}, &res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: detached\n", res.Device)
			return nil
		},
	}
}

func findUSB(st api.Status, host, device string) api.USBStatus {
	for _, u := range findHost(st, host).USB {
		if u.Device == device {
			return u
		}
	}
	return api.USBStatus{Device: device}
}

func newUSBIPHelperCmd() *cobra.Command {
	var socket, listen string
	var users []string
	var logLevel slog.Level
	cmd := &cobra.Command{
		Use:   "usbip-helper",
		Short: "Run the privileged USB/IP helper (as root)",
		Long: "Run the helper that shares USB devices for tether. It must run as root:\n" +
			"it moves devices to the usbip-host driver and hands connections to the\n" +
			"kernel. Run it as a system service; the daemon talks to it over --socket.\n\n" +
			"It exports only the devices a user's daemon asks for, only to that user's\n" +
			"connections, and only on loopback (remotes reach it through the SSH\n" +
			"connection). Devices come back to this machine when the daemon lets go\n" +
			"of them or disconnects.\n\n" +
			"  sudo tether usbip-helper --allow-user $USER\n\n" +
			"Needs the usbip-host kernel module (Fedora: kernel-modules-extra).\n\n" +
			"To run it as a systemd service: tether usbip-helper install",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var uids []int
			for _, u := range users {
				uid, err := lookupUID(u)
				if err != nil {
					return err
				}
				uids = append(uids, uid)
			}
			if len(uids) == 0 {
				return errors.New("name the users who may share devices with --allow-user")
			}
			return usbip.RunHelper(cmd.Context(), usbip.HelperOptions{
				Socket:    socket,
				Listen:    listen,
				AllowUIDs: uids,
				Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})),
			})
		},
	}
	cmd.AddCommand(newUSBIPInstallCmd(), newUSBIPUninstallCmd())
	cmd.Flags().StringVar(&socket, "socket", usbip.DefaultHelperSocket, "control socket for the daemon")
	cmd.Flags().StringVar(&listen, "listen", usbip.DefaultListen, "USB/IP address (IPv4 loopback)")
	cmd.Flags().StringArrayVar(&users, "allow-user", nil, "user name or uid allowed to share devices (repeatable)")
	cmd.Flags().TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, error")
	return cmd
}

func newUSBIPInstallCmd() *cobra.Command {
	var users []string
	var listen string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the helper as a systemd system service",
		Long: "Install the helper as the systemd system service tether-usbip, and start it:\n" +
			"copies this tether to " + usbip.InstalledPath + ", writes " + usbip.UnitPath + ",\n" +
			"and loads usbip-host at boot (" + usbip.ModulesPath + ").\n\n" +
			"Run it again after upgrading tether so the service uses the new version.\n" +
			"Asks for your password with sudo unless run as root. --allow-user defaults\n" +
			"to the user running it. Not for NixOS, where the\n" +
			"tether.nixosModules.usbip-helper module does this.",
		Example: "  tether usbip-helper install\n  tether usbip-helper install --allow-user alice --allow-user bob",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return rerunWithSudo(cmd)
			}
			if len(users) == 0 {
				// The uid, not the name: it needs no user database lookup.
				if uid := os.Getenv("SUDO_UID"); uid != "" && uid != "0" {
					users = []string{uid}
				}
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			if bin, err = filepath.EvalSymlinks(bin); err != nil {
				return err
			}
			in := &usbip.Installer{Binary: bin, Users: users, Listen: listen, Out: cmd.OutOrStdout()}
			return in.Install(cmd.Context())
		},
	}
	cmd.Flags().StringArrayVar(&users, "allow-user", nil, "user name or uid allowed to share devices (repeatable; default: the user running sudo)")
	cmd.Flags().StringVar(&listen, "listen", usbip.DefaultListen, "USB/IP address (IPv4 loopback)")
	return cmd
}

func newUSBIPUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the helper's systemd service",
		Long: "Stop and remove the tether-usbip service and the files `install` wrote.\n" +
			"Devices that are shared come back to this machine.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return rerunWithSudo(cmd)
			}
			return (&usbip.Installer{Out: cmd.OutOrStdout()}).Uninstall()
		},
	}
}

// rerunWithSudo runs this command again as root. It uses this binary's full
// path: sudo's secure_path usually lacks ~/.nix-profile/bin and the like.
func rerunWithSudo(cmd *cobra.Command) error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fmt.Errorf("%s needs root; run it as root", cmd.CommandPath())
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s needs root; running it with sudo\n", cmd.CommandPath())
	c := exec.CommandContext(cmd.Context(), sudo, append([]string{"--", exe}, os.Args[1:]...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			os.Exit(exitErr.ExitCode()) // sudo or the command already said why
		}
		return err
	}
	return nil
}

func lookupUID(name string) (int, error) {
	if uid, err := strconv.Atoi(name); err == nil {
		return uid, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

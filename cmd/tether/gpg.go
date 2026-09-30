package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
)

func newGPGCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gpg",
		Short: "Forward gpg-agent to a host",
		Long: "Forward your local gpg-agent to a host, so gpg there signs and decrypts\n" +
			"with your local keys. `on` and `off` are shortcuts for `tether fwd add|rm\n" +
			"HOST gpg-agent [gpg-ssh]`; to forward gpg permanently, set gpg (and\n" +
			"gpg_ssh) in a profile.\n\n" +
			"Several machines can forward their agents to the same host (say, your\n" +
			"laptop at home and your desktop at work); gpg there uses one at a time.\n" +
			"Connecting to the host, or turning gpg on, makes it use this machine's.\n" +
			"Reconnecting on its own (after sleep or a network change) doesn't take\n" +
			"it from a machine still using it: this one stands by, and `use` (or the\n" +
			"tray, or G in the terminal UI) switches to it. When the machine using it\n" +
			"disconnects, it passes to one standing by; if it vanishes without\n" +
			"disconnecting, one standing by takes over after a minute.\n\n" +
			"Run `tether doctor HOST` to check the remote's gpg setup.",
	}

	var ssh, noWait bool
	var timeout time.Duration
	on := &cobra.Command{
		Use:   "on HOST",
		Short: "Forward gpg-agent (and with --ssh, its SSH socket) to HOST",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return addForwards(cmd, g, args[0], gpgForwards(ssh), noWait, timeout)
		},
	}
	on.Flags().BoolVar(&ssh, "ssh", false, "also forward gpg-agent's SSH socket, letting the remote use your SSH keys")
	on.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the result")
	on.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "how long to wait")

	var offSSH bool
	off := &cobra.Command{
		Use:   "off HOST",
		Short: "Stop the ad-hoc gpg forwards to HOST",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeForwards(cmd, g, args[0], gpgForwards(offSSH))
		},
	}
	off.Flags().BoolVar(&offSSH, "ssh", false, "also stop the SSH socket forward")

	var useTimeout time.Duration
	use := &cobra.Command{
		Use:   "use HOST",
		Short: "Make gpg on HOST use this machine's keys, taking over from another machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return useGPG(cmd, g, args[0], useTimeout)
		},
	}
	use.Flags().DurationVar(&useTimeout, "timeout", 15*time.Second, "how long to wait")

	cmd.AddCommand(on, off, use)
	return cmd
}

func useGPG(cmd *cobra.Command, g *globalFlags, host string, timeout time.Duration) error {
	ctx := cmd.Context()
	g.noAutostart = true
	c, err := g.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	next, err := subscribe(ctx, c)
	if err != nil {
		return err
	}
	var res api.TargetResult
	if err := c.Call(ctx, api.MethodGPGClaim, api.GPGClaimParams{Host: host}, &res); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	st, err := waitUntil(wctx, next, res.Generation, func(st api.Status) bool {
		for _, f := range findHost(st, host).Forwards {
			if (f.Spec == "gpg-agent" || f.Spec == "gpg-ssh") && (f.State != api.StateUp || f.UsedBy != "") {
				return f.State == api.StateError
			}
		}
		return true
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("gpg on %s still doesn't use this machine's keys after %s; check `tether status`", host, timeout)
	}
	if err != nil {
		return err
	}
	for _, f := range findHost(st, host).Forwards {
		if f.State == api.StateError && (f.Spec == "gpg-agent" || f.Spec == "gpg-ssh") {
			return fmt.Errorf("%s: %s", f.Spec, f.Error)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "gpg on %s now uses this machine's keys\n", host)
	return nil
}

func gpgForwards(ssh bool) []string {
	if ssh {
		return []string{"gpg-agent", "gpg-ssh"}
	}
	return []string{"gpg-agent"}
}

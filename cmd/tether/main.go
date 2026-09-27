// Command tether manages port forwards, gpg-agent forwarding and mounts for
// remote development. `tether daemon` runs the background service; every
// other subcommand is a client of it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/client"
	"github.com/ryanmwright/tether/internal/paths"
	"github.com/ryanmwright/tether/internal/rpc"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type globalFlags struct {
	socket      string
	config      string
	noAutostart bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "tether:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "tether",
		Short:         "Forwarding manager for remote development",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.socket, "socket", envOr("TETHER_SOCKET", paths.SocketPath()), "daemon socket path ($TETHER_SOCKET)")
	pf.StringVar(&g.config, "config", envOr("TETHER_CONFIG", paths.ConfigFile()), "config file path ($TETHER_CONFIG)")
	pf.BoolVar(&g.noAutostart, "no-autostart", false, "don't start the daemon if it isn't running")

	root.AddCommand(
		newDaemonCmd(g),
		newStatusCmd(g),
		newUpCmd(g),
		newDownCmd(g),
		newFwdCmd(g),
		newGPGCmd(g),
		newMountCmd(g),
		newHostCmd(g),
		newDoctorCmd(g),
		newLogsCmd(g),
		newTUICmd(g),
		newTrayCmd(g),
		newConfigCmd(g),
		&cobra.Command{
			Use:   "version",
			Short: "Print the tether version",
			Args:  cobra.NoArgs,
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), version) },
		},
	)
	return root
}

// connect connects to the daemon, starting it if needed, and checks that it
// speaks this tether's API.
func (g *globalFlags) connect(ctx context.Context) (*rpc.Client, error) {
	c, err := g.connectAny(ctx)
	if err != nil {
		return nil, err
	}
	var st api.Status
	if err := c.Call(ctx, api.MethodStatus, nil, &st); err != nil {
		c.Close()
		return nil, err
	}
	if st.Protocol < api.ProtocolVersion {
		c.Close()
		return nil, fmt.Errorf("the running daemon (pid %d, version %s) is older than this tether (%s).\n"+
			"Restart it: `tether daemon stop` (the next command starts the new one), or\n"+
			"`systemctl --user restart tether` if it runs as a service", st.PID, st.Version, version)
	}
	return c, nil
}

// connectAny connects without checking the daemon's version.
func (g *globalFlags) connectAny(ctx context.Context) (*rpc.Client, error) {
	c, err := client.Connect(ctx, client.Options{
		SocketPath: g.socket,
		ConfigPath: g.config,
		Autostart:  !g.noAutostart,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("daemon is not running (socket %s)", g.socket)
		}
		return nil, err
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/daemon"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/paths"
	"github.com/ryanmwright/tether/internal/usbip"
)

func newDaemonCmd(g *globalFlags) *cobra.Command {
	var logLevel slog.Level
	var sshConfig, usbHelper string
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the tether daemon in the foreground",
		Long: "Run the tether daemon in the foreground. Normally it is started by the\n" +
			"systemd user unit, or automatically by any other tether command.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
			return daemon.Run(cmd.Context(), daemon.Options{
				SocketPath:      g.socket,
				ConfigPath:      g.config,
				Version:         version,
				Logger:          log,
				SSH:             openssh.Options{ConfigFile: sshConfig},
				USBHelperSocket: usbHelper,
				RecentFile:      filepath.Join(paths.StateDir(), "recent.json"),
			})
		},
	}
	cmd.Flags().TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, error")
	cmd.Flags().StringVar(&sshConfig, "ssh-config", "", "ssh config file to use instead of ~/.ssh/config (ssh -F)")
	cmd.Flags().StringVar(&usbHelper, "usbip-helper-socket", usbip.DefaultHelperSocket, "the USB/IP helper's control socket")

	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			g.noAutostart = true
			c, err := g.connectAny(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			return c.Call(cmd.Context(), api.MethodShutdown, nil, nil)
		},
	})
	return cmd
}

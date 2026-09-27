package main

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"tether/internal/api"
	"tether/internal/daemon"
)

func newDaemonCmd(g *globalFlags) *cobra.Command {
	var logLevel slog.Level
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the tether daemon in the foreground",
		Long: "Run the tether daemon in the foreground. Normally it is started by the\n" +
			"systemd user unit, or automatically by any other tether command.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
			return daemon.Run(cmd.Context(), daemon.Options{
				SocketPath: g.socket,
				ConfigPath: g.config,
				Version:    version,
				Logger:     log,
			})
		},
	}
	cmd.Flags().TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, error")

	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			g.noAutostart = true
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			return c.Call(cmd.Context(), api.MethodShutdown, nil, nil)
		},
	})
	return cmd
}

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/rpc"
	"github.com/ryanmwright/tether/internal/tray"
)

func newTrayCmd(g *globalFlags) *cobra.Command {
	var terminal []string
	var noNotify bool
	var logLevel slog.Level
	cmd := &cobra.Command{
		Use:   "tray",
		Short: "Show the system tray icon",
		Long: "Show a system tray icon (StatusNotifierItem: KDE, and most other Linux\n" +
			"desktops) with the overall state, a menu to connect and disconnect hosts,\n" +
			"activate profiles and share USB devices, and desktop notifications when\n" +
			"connections drop or forwards fail. Left-click opens the terminal UI. Quitting the tray leaves\n" +
			"everything running in the daemon.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			return tray.Run(cmd.Context(), tray.Options{
				Connect: func(ctx context.Context, autostart bool) (*rpc.Client, error) {
					g.noAutostart = !autostart
					return g.connect(ctx)
				},
				TetherPath: exe,
				Terminal:   terminal,
				Notify:     !noNotify,
				Log:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})),
			})
		},
	}
	cmd.Flags().StringArrayVar(&terminal, "terminal", nil, "terminal command for the TUI, one word per flag, e.g. --terminal konsole --terminal -e (default: detect)")
	cmd.Flags().BoolVar(&noNotify, "no-notify", false, "don't show desktop notifications")
	cmd.Flags().TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, error")
	return cmd
}

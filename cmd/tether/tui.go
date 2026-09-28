package main

import (
	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/tui"
)

func newTUICmd(g *globalFlags) *cobra.Command {
	var pvcHost string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive terminal UI",
		Long: "Open the interactive terminal UI: live status of hosts, forwards and\n" +
			"profiles, with keys to bring them up and down, add forwards, run doctor\n" +
			"and follow the log. Quitting leaves everything running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := g.connect(ctx)
			if err != nil {
				return err
			}
			defer c.Close()
			if err := c.Call(ctx, api.MethodSubscribe, api.SubscribeParams{Logs: true}, nil); err != nil {
				return err
			}
			var logs []api.LogEntry
			if err := c.Call(ctx, api.MethodLogs, api.LogsParams{Limit: 200}, &logs); err != nil {
				return err
			}
			m := tui.New(c, logs)
			if pvcHost != "" {
				m = m.WithPVCPicker(pvcHost)
			}
			_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
			return err
		},
	}
	cmd.Flags().StringVar(&pvcHost, "pvc", "", "open the Kubernetes claim picker for this host")
	return cmd
}

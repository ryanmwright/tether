package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
)

func newLogsCmd(g *globalFlags) *cobra.Command {
	var follow bool
	var limit int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the daemon's recent log",
		Long: "Show the daemon's recent log (it keeps the last 500 entries). The full\n" +
			"log is in the journal (journalctl --user -u tether) or daemon.log.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			g.noAutostart = true
			c, err := g.connect(ctx)
			if err != nil {
				return err
			}
			defer c.Close()
			out := cmd.OutOrStdout()

			// Subscribe before fetching history so nothing falls in between;
			// entries seen in both are skipped below.
			if follow {
				if err := c.Call(ctx, api.MethodSubscribe, api.SubscribeParams{Logs: true}, nil); err != nil {
					return err
				}
			}
			var entries []api.LogEntry
			if err := c.Call(ctx, api.MethodLogs, api.LogsParams{Limit: limit}, &entries); err != nil {
				return err
			}
			for _, e := range entries {
				fmt.Fprintln(out, e.Line())
			}
			if !follow {
				return nil
			}
			var last api.LogEntry
			if len(entries) > 0 {
				last = entries[len(entries)-1]
			}
			for {
				select {
				case <-ctx.Done():
					return nil
				case n, ok := <-c.Notifications():
					if !ok {
						return fmt.Errorf("daemon closed the connection")
					}
					if n.Method != api.EventLog {
						continue
					}
					var e api.LogEntry
					if err := json.Unmarshal(n.Params, &e); err != nil {
						return err
					}
					if !e.Time.After(last.Time) {
						continue // already printed from history
					}
					fmt.Fprintln(out, e.Line())
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new entries")
	cmd.Flags().IntVarP(&limit, "lines", "n", 50, "how many recent entries to show (0 for all)")
	return cmd
}

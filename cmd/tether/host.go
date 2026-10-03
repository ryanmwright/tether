package main

import (
	"cmp"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
)

func newHostCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Connect to hosts that aren't in the config",
		Long: "Add or remove ad-hoc hosts: hosts that aren't in the config file. They\n" +
			"work like configured hosts (forwards, mounts, gpg, doctor) until removed\n" +
			"or the daemon exits; add them to the config to keep them.",
	}

	var noWait bool
	var timeout time.Duration
	var displayName string
	add := &cobra.Command{
		Use:   "add NAME [SSH-DEST]",
		Short: "Add a host and connect to it",
		Long: "Add a host and connect to it. SSH-DEST is what you'd pass to ssh: an alias\n" +
			"from ~/.ssh/config or user@host. It defaults to NAME, so an ssh alias\n" +
			"needs no destination:\n\n" +
			"  tether host add devbox2\n" +
			"  tether host add scratch me@10.0.0.5",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.HostParams{Name: args[0], DisplayName: displayName}
			if len(args) == 2 {
				p.SSH = args[1]
			}
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
			var res api.TargetResult
			if err := c.Call(ctx, api.MethodHostAdd, p, &res); err != nil {
				return err
			}
			if noWait {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: starting\n", cmp.Or(displayName, res.Name))
				return nil
			}
			return waitTargets(cmd, next, []api.TargetResult{res}, timeout)
		},
	}
	add.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the result")
	add.Flags().StringVar(&displayName, "display-name", "", "name to show for it in the tray and terminal UI")
	add.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "how long to wait")

	rm := &cobra.Command{
		Use:   "rm NAME...",
		Short: "Disconnect ad-hoc hosts and forget them, with their forwards and mounts",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g.noAutostart = true
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			for _, name := range args {
				if err := c.Call(cmd.Context(), api.MethodHostRemove, api.HostParams{Name: name}, nil); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: removed\n", name)
			}
			return nil
		},
	}
	cmd.AddCommand(add, rm)
	return cmd
}

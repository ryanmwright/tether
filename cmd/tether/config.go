package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/config"
)

func newConfigCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Validate or reload the config file",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "check",
			Short: "Validate the config file without contacting the daemon",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: ok (%d hosts, %d profiles)\n", g.config, len(cfg.Hosts), len(cfg.Profiles))
				return nil
			},
		},
		&cobra.Command{
			Use:   "reload",
			Short: "Tell the daemon to re-read the config file",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := g.connect(cmd.Context())
				if err != nil {
					return err
				}
				defer c.Close()
				var res api.ReloadResult
				if err := c.Call(cmd.Context(), api.MethodReload, nil, &res); err != nil {
					return fmt.Errorf("reload failed, daemon kept previous config:\n%w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "reloaded (%d hosts, %d profiles)\n", res.Hosts, res.Profiles)
				return nil
			},
		},
	)
	return cmd
}

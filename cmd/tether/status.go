package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tether/internal/api"
)

func newStatusCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon, host and profile status",
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
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			printStatus(cmd, &st)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func printStatus(cmd *cobra.Command, st *api.Status) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "daemon  %s  pid %d  up %s\n", st.Version, st.PID, time.Since(st.StartedAt).Round(time.Second))
	fmt.Fprintf(out, "config  %s\n", st.ConfigPath)
	if st.ConfigError != "" {
		fmt.Fprintf(out, "        error (using last good config):\n")
		for line := range strings.SplitSeq(st.ConfigError, "\n") {
			fmt.Fprintf(out, "          %s\n", line)
		}
	}

	if len(st.Hosts) == 0 {
		fmt.Fprintln(out, "\nno hosts configured")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nHOST\tSSH\tAUTO\tSTATE")
	for _, h := range st.Hosts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.Name, h.SSH, yesNo(h.Autoconnect), stateText(h.State, h.Error))
	}
	if len(st.Profiles) > 0 {
		fmt.Fprintln(tw, "\nPROFILE\tHOST\tAUTO\tSTATE")
		for _, p := range st.Profiles {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, p.Host, yesNo(p.Autoconnect), stateText(p.State, p.Error))
		}
	}
	tw.Flush()
}

func stateText(s api.State, errMsg string) string {
	if errMsg != "" {
		return fmt.Sprintf("%s: %s", s, errMsg)
	}
	return string(s)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

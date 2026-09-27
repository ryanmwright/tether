package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
)

func newDoctorCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor HOST",
		Short: "Check that a host is set up correctly",
		Long: "Check the local setup, the connection and the remote's gpg setup for a\n" +
			"host, and suggest fixes. It only inspects; nothing is changed. If the host\n" +
			"isn't connected, doctor connects briefly just for the checks.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.DoctorResult
			if err := c.Call(cmd.Context(), api.MethodDoctor, api.DoctorParams{Host: args[0]}, &res); err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			if printChecks(cmd.OutOrStdout(), res.Checks) {
				return errors.New("some checks failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

// printChecks prints checks grouped by section and reports whether any
// failed.
func printChecks(out io.Writer, checks []api.Check) (failed bool) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	section := ""
	for _, c := range checks {
		if c.Section != section {
			if section != "" {
				fmt.Fprintln(tw)
			}
			section = c.Section
			fmt.Fprintln(tw, section)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", checkMark(c.Status), c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(tw, "  \t\tfix: %s\n", c.Fix)
		}
		failed = failed || c.Status == api.CheckFail
	}
	return failed
}

func checkMark(s api.CheckStatus) string {
	switch s {
	case api.CheckOK:
		return "ok"
	case api.CheckWarn:
		return "WARN"
	case api.CheckFail:
		return "FAIL"
	default:
		return "skip"
	}
}

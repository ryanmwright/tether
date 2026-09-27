package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/forward"
)

func newStatusCmd(g *globalFlags) *cobra.Command {
	var asJSON, watch bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show hosts, forwards and profiles",
		Long: "Show hosts, forwards and profiles. With --watch, redraw on every change;\n" +
			"with --watch --json, print one JSON object per line on every change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := g.connect(ctx)
			if err != nil {
				return err
			}
			defer c.Close()
			out := cmd.OutOrStdout()

			if !watch {
				var st api.Status
				if err := c.Call(ctx, api.MethodStatus, nil, &st); err != nil {
					return err
				}
				if asJSON {
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(st)
				}
				printStatus(out, &st)
				return nil
			}

			next, err := subscribe(ctx, c)
			if err != nil {
				return err
			}
			for {
				st, err := next(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return nil // interrupted
					}
					return err
				}
				if asJSON {
					if err := json.NewEncoder(out).Encode(st); err != nil {
						return err
					}
					continue
				}
				fmt.Fprint(out, "\x1b[H\x1b[2J") // home, clear screen
				printStatus(out, &st)
			}
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "keep running and show every change")
	return cmd
}

func printStatus(out io.Writer, st *api.Status) {
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
	defer tw.Flush()
	fmt.Fprintln(tw, "\nHOST\tSSH\tAUTO\tSTATE\tDETAIL")
	var forwards bool
	for _, h := range st.Hosts {
		detail := h.Error
		if h.RetryAt != nil {
			detail += fmt.Sprintf(" (retry in %s)", untilRounded(*h.RetryAt))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Name, h.SSH, yesNo(h.Autoconnect), h.State, detail)
		forwards = forwards || len(h.Forwards) > 0
	}

	if forwards {
		fmt.Fprintln(tw, "\nFORWARD\tHOST\tFROM\tSTATE\tDETAIL")
		for _, h := range st.Hosts {
			for _, f := range h.Forwards {
				detail := f.Error
				switch {
				case f.AllocatedPort != 0:
					detail = fmt.Sprintf("remote port %d", f.AllocatedPort)
				case f.Resolved != "":
					if spec, err := forward.Parse(f.Resolved); err == nil {
						detail = "remote " + spec.Listen.String()
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Spec, h.Name, forwardSource(f), f.State, detail)
			}
		}
	}

	var mounts bool
	for _, h := range st.Hosts {
		mounts = mounts || len(h.Mounts) > 0
	}
	if mounts {
		fmt.Fprintln(tw, "\nMOUNT\tHOST\tFROM\tSTATE\tDETAIL")
		for _, h := range st.Hosts {
			for _, m := range h.Mounts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Key, h.Name, source(m.Profiles, m.AdHoc), m.State, m.Error)
			}
		}
	}

	if len(st.Profiles) > 0 {
		fmt.Fprintln(tw, "\nPROFILE\tHOST\tAUTO\tSTATE\tDETAIL")
		for _, p := range st.Profiles {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Host, yesNo(p.Autoconnect), p.State, p.Error)
		}
	}
}

func forwardSource(f api.ForwardStatus) string { return source(f.Profiles, f.AdHoc) }

// source says where a forward or mount comes from: its profiles, and
// "ad-hoc" if added by hand.
func source(profiles []string, adhoc bool) string {
	sources := slices.Clip(profiles)
	if adhoc {
		sources = append(sources, "ad-hoc")
	}
	return strings.Join(sources, ",")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

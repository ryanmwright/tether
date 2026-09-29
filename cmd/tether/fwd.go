package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/forward"
)

func newFwdCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fwd",
		Short: "Add or remove ad-hoc forwards",
		Long: "Add or remove ad-hoc forwards. They last until removed, the host is\n" +
			"taken down, or the daemon exits; put permanent ones in a profile.\n\n" +
			"What you can forward (HOST is the host you're connected to):\n" +
			"  5432                          localhost:5432 here -> port 5432 on HOST\n" +
			"  8080:3000                     localhost:8080 here -> port 3000 on HOST\n" +
			"  db.internal:5432              localhost:5432 here -> db.internal:5432, a machine HOST reaches\n" +
			"  L:[bind:]port:host:hostport   the same, in full; port 0 picks a free one\n" +
			"  R:[bind:]port:host:hostport   port on HOST -> host:hostport reached from here\n" +
			"                                (localhost: this machine; any other name: a machine on your network)\n" +
			"  socks, D:[bind:]port          SOCKS proxy here, connecting out from HOST\n" +
			"  rsocks, R:[bind:]port         SOCKS proxy on HOST, connecting out from here\n" +
			"  http, H:[bind:]port           HTTP (and SOCKS) proxy here, connecting out from HOST\n" +
			"  K:[bind:]port:[CONTEXT/]NS/KIND/NAME:PORT\n" +
			"                                a Kubernetes svc, pod, deploy or sts, via kubectl on HOST\n" +
			"                                (see `tether kube fwd`)\n" +
			"  gpg-agent, gpg-ssh            your gpg-agent and its SSH socket (see `tether gpg`)\n" +
			"Either side of L and R may be a Unix socket path, e.g. L:2375:/var/run/docker.sock.\n" +
			"Name a forward with LABEL=, e.g. postgres=db.internal:5432.\n\n" +
			"`tether fwd explain SPEC` says what a spec does without adding it.",
	}
	cmd.AddCommand(newFwdAddCmd(g), newFwdRmCmd(g), newFwdLsCmd(g), newFwdExplainCmd())
	return cmd
}

func newFwdAddCmd(g *globalFlags) *cobra.Command {
	var noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "add HOST [LABEL=]SPEC...",
		Short: "Add forwards to a host, connecting it if needed (see `tether fwd --help` for what SPEC can be)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return addForwards(cmd, g, args[0], args[1:], noWait, timeout)
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the result")
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "how long to wait")
	return cmd
}

func newFwdRmCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rm HOST SPEC...",
		Short: "Remove ad-hoc forwards",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeForwards(cmd, g, args[0], args[1:])
		},
	}
}

func newFwdLsCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "ls [HOST]",
		Short: "List forwards, what they do, and where to connect",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g.noAutostart = true
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			var st api.Status
			if err := c.Call(cmd.Context(), api.MethodStatus, nil, &st); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "HOST\tNAME\tSTATE\tCONNECT TO\tWHAT")
			n := 0
			for _, h := range st.Hosts {
				if len(args) == 1 && h.Name != args[0] {
					continue
				}
				for _, f := range h.Forwards {
					n++
					name := cmp.Or(f.Label, f.Spec)
					state := string(f.State)
					if f.Target == "unreachable" {
						state += " (target unreachable)"
					}
					what := cmp.Or(f.Error, f.TargetError, f.Description, f.Resolved)
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Name, name, state, dash(f.Address), what)
				}
			}
			if n == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no forwards; add one with `tether fwd add HOST SPEC` (see `tether fwd --help`)")
				return nil
			}
			return tw.Flush()
		},
	}
}

func newFwdExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain SPEC...",
		Short: "Say what a forward spec, shorthand or name does, without adding it",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				label, spec, err := forward.ParseInput(a)
				if err != nil {
					return err
				}
				name := spec.String()
				if label != "" {
					name = label + " = " + name
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n  %s\n", name, forward.Describe(spec, "HOST"))
			}
			return nil
		},
	}
}

func removeForwards(cmd *cobra.Command, g *globalFlags, host string, specs []string) error {
	g.noAutostart = true
	c, err := g.connect(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()
	for _, spec := range specs {
		var res api.ForwardResult
		if err := c.Call(cmd.Context(), api.MethodForwardRemove, api.ForwardParams{Host: host, Spec: spec}, &res); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: removed\n", res.Spec)
	}
	return nil
}

// addForwards adds forwards to host and, unless noWait, waits for and
// reports the outcome of each.
func addForwards(cmd *cobra.Command, g *globalFlags, host string, specs []string, noWait bool, timeout time.Duration) error {
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
	var results []api.ForwardResult
	for _, spec := range specs {
		var res api.ForwardResult
		if err := c.Call(ctx, api.MethodForwardAdd, api.ForwardParams{Host: host, Spec: spec}, &res); err != nil {
			return err
		}
		results = append(results, res)
	}
	if noWait {
		for _, r := range results {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: added\n", r.Spec)
		}
		return nil
	}

	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	st, err := waitUntil(wctx, next, results[len(results)-1].Generation, func(st api.Status) bool {
		if findHost(st, host).State == api.StateError {
			return true
		}
		for _, r := range results {
			if !settled(findForward(st, host, r.Spec).State) {
				return false
			}
		}
		return true
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("still not up after %s; check `tether status`", timeout)
	}
	if err != nil {
		return err
	}

	if h := findHost(st, host); h.State == api.StateError {
		return fmt.Errorf("%s: %s (forwards are kept and will be added once connected)", host, h.Error)
	}
	healthy := true
	for _, r := range results {
		f := findForward(st, host, r.Spec)
		name := f.Spec
		if f.Label != "" {
			name = f.Label + " (" + f.Spec + ")"
		}
		switch {
		case f.State == api.StateError:
			healthy = false
			fmt.Fprintf(cmd.OutOrStdout(), "%s: error: %s\n", name, f.Error)
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", name, f.State)
			if f.Description != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", f.Description)
			}
		}
	}
	if !healthy {
		fmt.Fprintf(cmd.ErrOrStderr(), "failed forwards are kept and retried; remove them with `tether fwd rm %s SPEC`\n", host)
		return errNotHealthy
	}
	return nil
}

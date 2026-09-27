package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
)

func newFwdCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fwd",
		Short: "Add or remove ad-hoc forwards",
		Long: "Add or remove ad-hoc forwards. They last until removed, the host is\n" +
			"taken down, or the daemon exits; put permanent ones in a profile.\n\n" +
			"Specs use ssh's -L/-R/-D syntax with a kind prefix:\n" +
			"  L:[bind:]port:host:hostport   local port -> host:hostport from the remote\n" +
			"  R:[bind:]port:host:hostport   remote port -> host:hostport from here\n" +
			"  D:[bind:]port                 SOCKS proxy on a local port\n" +
			"Either side may be a Unix socket path instead, e.g. L:2375:/var/run/docker.sock.\n\n" +
			"The names gpg-agent and gpg-ssh forward your gpg-agent and its SSH socket\n" +
			"(see `tether gpg`).",
	}
	cmd.AddCommand(newFwdAddCmd(g), newFwdRmCmd(g))
	return cmd
}

func newFwdAddCmd(g *globalFlags) *cobra.Command {
	var noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "add HOST SPEC...",
		Short: "Add forwards to a host, connecting it if needed",
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
		switch {
		case f.State == api.StateError:
			healthy = false
			fmt.Fprintf(cmd.OutOrStdout(), "%s: error: %s\n", f.Spec, f.Error)
		case f.AllocatedPort != 0:
			fmt.Fprintf(cmd.OutOrStdout(), "%s: up (remote port %d)\n", f.Spec, f.AllocatedPort)
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", f.Spec, f.State)
		}
	}
	if !healthy {
		fmt.Fprintf(cmd.ErrOrStderr(), "failed forwards are kept and retried; remove them with `tether fwd rm %s SPEC`\n", host)
		return errNotHealthy
	}
	return nil
}

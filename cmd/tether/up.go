package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
)

type targetFlags struct {
	host, profile bool
}

func (f *targetFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.host, "host", false, "treat names as hosts")
	cmd.Flags().BoolVar(&f.profile, "profile", false, "treat names as profiles")
	cmd.MarkFlagsMutuallyExclusive("host", "profile")
}

func (f *targetFlags) params(name string) api.TargetParams {
	p := api.TargetParams{Name: name}
	switch {
	case f.host:
		p.Kind = api.TargetHost
	case f.profile:
		p.Kind = api.TargetProfile
	}
	return p
}

// errNotHealthy makes the command exit non-zero after its output explains
// what's wrong.
var errNotHealthy = errors.New("not everything came up")

func newUpCmd(g *globalFlags) *cobra.Command {
	var tf targetFlags
	var noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "up NAME...",
		Short: "Connect hosts or activate profiles",
		Long: "Connect hosts or activate profiles. A profile brings up its host and\n" +
			"forwards. Waits until each is up or has failed its first attempt; failed\n" +
			"connections keep retrying in the background.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, names []string) error {
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
			var results []api.TargetResult
			for _, name := range names {
				var res api.TargetResult
				if err := c.Call(ctx, api.MethodUp, tf.params(name), &res); err != nil {
					return err
				}
				results = append(results, res)
			}
			if noWait {
				for _, r := range results {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: starting\n", r.Name)
				}
				return nil
			}

			wctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			gen := results[len(results)-1].Generation
			st, err := waitUntil(wctx, next, gen, func(st api.Status) bool {
				for _, r := range results {
					if !settled(targetState(st, r)) {
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
			healthy := true
			for _, r := range results {
				healthy = printTarget(cmd.OutOrStdout(), st, r) && healthy
			}
			if !healthy {
				return errNotHealthy
			}
			return nil
		},
	}
	tf.register(cmd)
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return without waiting for the result")
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "how long to wait")
	return cmd
}

func newDownCmd(g *globalFlags) *cobra.Command {
	var tf targetFlags
	cmd := &cobra.Command{
		Use:   "down NAME...",
		Short: "Disconnect hosts or deactivate profiles",
		Long: "Deactivate profiles, or disconnect hosts. Taking a host down also\n" +
			"deactivates its profiles and removes its ad-hoc forwards.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, names []string) error {
			g.noAutostart = true
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			for _, name := range names {
				var res api.TargetResult
				if err := c.Call(cmd.Context(), api.MethodDown, tf.params(name), &res); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: down\n", res.Name)
			}
			return nil
		},
	}
	tf.register(cmd)
	return cmd
}

func targetState(st api.Status, r api.TargetResult) api.State {
	if r.Kind == api.TargetProfile {
		return findProfile(st, r.Name).State
	}
	return findHost(st, r.Host).State
}

// printTarget prints one line for r and reports whether it is fully up.
func printTarget(w io.Writer, st api.Status, r api.TargetResult) bool {
	h := findHost(st, r.Host)
	state, detail := h.State, h.Error
	if r.Kind == api.TargetProfile {
		p := findProfile(st, r.Name)
		state, detail = p.State, p.Error
	}
	if state == api.StateError && h.RetryAt != nil {
		detail += fmt.Sprintf(" (retrying in %s)", untilRounded(*h.RetryAt))
	}
	if detail != "" {
		fmt.Fprintf(w, "%s: %s: %s\n", r.Name, state, detail)
	} else {
		fmt.Fprintf(w, "%s: %s\n", r.Name, state)
	}
	return state == api.StateUp
}

func untilRounded(t time.Time) time.Duration {
	return max(time.Until(t).Round(time.Second), 0)
}

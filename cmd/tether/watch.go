package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/rpc"
)

type nextFunc func(ctx context.Context) (api.Status, error)

// subscribe starts status events on c and returns a function yielding each
// snapshot in turn.
func subscribe(ctx context.Context, c *rpc.Client) (nextFunc, error) {
	if err := c.Call(ctx, api.MethodSubscribe, api.SubscribeParams{}, nil); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (api.Status, error) {
		for {
			select {
			case n, ok := <-c.Notifications():
				if !ok {
					return api.Status{}, errors.New("daemon closed the connection")
				}
				if n.Method != api.EventStatus {
					continue
				}
				var st api.Status
				if err := json.Unmarshal(n.Params, &st); err != nil {
					return api.Status{}, fmt.Errorf("decoding status: %w", err)
				}
				return st, nil
			case <-ctx.Done():
				return api.Status{}, ctx.Err()
			}
		}
	}, nil
}

// settled reports whether a state is a final answer for a waiting command.
func settled(s api.State) bool {
	return s == api.StateUp || s == api.StateDegraded || s == api.StateError
}

// waitUntil reads snapshots at least as new as gen until done returns true,
// and returns that snapshot.
func waitUntil(ctx context.Context, next nextFunc, gen uint64, done func(api.Status) bool) (api.Status, error) {
	for {
		st, err := next(ctx)
		if err != nil {
			return st, err
		}
		if st.Generation >= gen && done(st) {
			return st, nil
		}
	}
}

func findHost(st api.Status, name string) api.HostStatus {
	for _, h := range st.Hosts {
		if h.Name == name {
			return h
		}
	}
	return api.HostStatus{Name: name}
}

func findProfile(st api.Status, name string) api.ProfileStatus {
	for _, p := range st.Profiles {
		if p.Name == name {
			return p
		}
	}
	return api.ProfileStatus{Name: name}
}

func findForward(st api.Status, host, spec string) api.ForwardStatus {
	for _, f := range findHost(st, host).Forwards {
		if f.Spec == spec {
			return f
		}
	}
	return api.ForwardStatus{Spec: spec}
}

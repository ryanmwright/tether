package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/mount"
)

func newMountCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mount",
		Short: "Add or remove ad-hoc directory mounts",
		Long: "Add or remove ad-hoc directory mounts. Mark the remote side with \"remote:\",\n" +
			"like scp:\n\n" +
			"  tether mount add devbox remote:~/src ~/mnt/src   the remote's ~/src, here\n" +
			"  tether mount add devbox ~/proj remote:~/proj     your ~/proj, on the remote\n" +
			"  tether mount add jump pvc:prod/db/data [DST]     a Kubernetes claim, here (see `tether kube`)\n\n" +
			"Remote paths are relative to the remote home unless absolute. Mounting a\n" +
			"local directory on the remote gives the remote access to that directory\n" +
			"only. Mounts need sshfs and FUSE: here for remote directories, on the\n" +
			"remote for local ones. For permanent mounts, use a profile.",
	}
	cmd.AddCommand(newMountAddCmd(g), newMountRmCmd(g))
	return cmd
}

// mountParams turns "HOST SRC DST" into API params, making a relative local
// path absolute (the daemon doesn't share our working directory). DST may be
// empty for a claim, for its default mount point.
func mountParams(host, src, dst string, options []string) (api.MountParams, error) {
	spec, err := mount.Parse(src, dst)
	if err != nil {
		return api.MountParams{}, err
	}
	if spec.Local == "" && spec.Direction != mount.PVCToLocal {
		return api.MountParams{}, errors.New("missing mount point")
	}
	if spec.Local != "" && spec.Local != "~" && !strings.HasPrefix(spec.Local, "~/") && !filepath.IsAbs(spec.Local) {
		if spec.Local, err = filepath.Abs(spec.Local); err != nil {
			return api.MountParams{}, err
		}
	}
	p := api.MountParams{Host: host, Direction: string(spec.Direction), Remote: spec.Remote, Local: spec.Local, Options: options}
	if k := spec.Kube; k != nil {
		p.Kube = &api.KubeMount{Context: k.Context, Namespace: k.Namespace, PVC: k.PVC, ReadOnly: slices.Contains(options, "ro")}
	}
	return p, nil
}

func newMountAddCmd(g *globalFlags) *cobra.Command {
	var options []string
	var wait waitFlags
	cmd := &cobra.Command{
		Use:   "add HOST SRC DST",
		Short: "Mount SRC at DST (one of them remote:PATH, or SRC pvc:CLAIM), connecting HOST if needed",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := ""
			if len(args) == 3 {
				dst = args[2]
			}
			params, err := mountParams(args[0], args[1], dst, options)
			if err != nil {
				return err
			}
			return addMount(cmd, g, params, wait)
		},
	}
	cmd.Flags().StringArrayVarP(&options, "option", "o", nil, "extra sshfs option, e.g. -o ro (repeatable)")
	wait.register(cmd)
	return cmd
}

type waitFlags struct {
	noWait  bool
	timeout time.Duration
}

func (w *waitFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&w.noWait, "no-wait", false, "return without waiting for the result")
	cmd.Flags().DurationVar(&w.timeout, "timeout", 3*time.Minute, "how long to wait")
}

// addMount asks the daemon for a mount and waits until it's up or failed.
func addMount(cmd *cobra.Command, g *globalFlags, params api.MountParams, wait waitFlags) error {
	ctx := cmd.Context()
	c, err := g.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	var next nextFunc
	if !wait.noWait {
		if next, err = subscribe(ctx, c); err != nil {
			return err
		}
	}
	var res api.MountResult
	if err := c.Call(ctx, api.MethodMountAdd, params, &res); err != nil {
		return err
	}
	if wait.noWait {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: added\n", res.Key)
		return nil
	}

	wctx, cancel := context.WithTimeout(ctx, wait.timeout)
	defer cancel()
	st, err := waitUntil(wctx, next, res.Generation, func(st api.Status) bool {
		return findHost(st, res.Host).State == api.StateError || settled(findMount(st, res.Host, res.Key).State)
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("still not mounted after %s; check `tether status`", wait.timeout)
	}
	if err != nil {
		return err
	}
	if h := findHost(st, res.Host); h.State == api.StateError {
		return fmt.Errorf("%s: %s (the mount is kept and will be made once connected)", res.Host, h.Error)
	}
	m := findMount(st, res.Host, res.Key)
	if m.State == api.StateError {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: error: %s\n", res.Key, m.Error)
		fmt.Fprintf(cmd.ErrOrStderr(), "failed mounts are kept and retried; remove with `tether mount rm %s`\n", mountRmArgs(res.Host, m))
		return errNotHealthy
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: up\n", res.Key)
	if m.Kube != nil && m.Kube.Pod != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "helper pod %s/%s on %s\n", m.Kube.Namespace, m.Kube.Pod, cmp.Or(m.Kube.Node, "?"))
	}
	return nil
}

// mountRmArgs are the arguments to `tether mount rm` for m.
func mountRmArgs(host string, m api.MountStatus) string {
	switch {
	case m.Kube != nil:
		return host + " " + m.Remote + " " + m.Local
	case m.Direction == string(mount.LocalToRemote):
		return host + " " + m.Local + " " + mount.RemotePrefix + m.Remote
	}
	return host + " " + mount.RemotePrefix + m.Remote + " " + m.Local
}

func newMountRmCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rm HOST SRC DST",
		Short: "Unmount and remove an ad-hoc mount",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := ""
			if len(args) == 3 {
				dst = args[2]
			}
			params, err := mountParams(args[0], args[1], dst, nil)
			if err != nil {
				return err
			}
			g.noAutostart = true
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.MountResult
			if err := c.Call(cmd.Context(), api.MethodMountRemove, params, &res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: removed\n", res.Key)
			return nil
		},
	}
}

func findMount(st api.Status, host, key string) api.MountStatus {
	for _, m := range findHost(st, host).Mounts {
		if m.Key == key {
			return m
		}
	}
	return api.MountStatus{Key: key}
}

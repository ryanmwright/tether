package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/mount"
)

func newKubeCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kube",
		Short: "Mount Kubernetes PersistentVolumeClaims",
		Long: "Mount Kubernetes PersistentVolumeClaims here. kubectl runs on HOST, over its\n" +
			"connection: typically a jump box with access to the cluster. Use the\n" +
			"built-in host \"local\" to run kubectl on this machine instead (when tether\n" +
			"runs on the jump box itself).\n\n" +
			"  tether kube ls jump                      claims in the current context\n" +
			"  tether kube mount jump db/data-pg-0      mount at ~/mnt/k8s/<context>/db/data-pg-0\n" +
			"  tether kube mount jump prod/db/data ~/pg --ro\n" +
			"  tether kube umount jump db/data-pg-0\n\n" +
			"A small helper pod mounts the claim and serves it through `kubectl exec`;\n" +
			"nothing listens on a port. It's pinned to the node of a pod already using\n" +
			"a ReadWriteOnce volume, and it provisions a WaitForFirstConsumer volume\n" +
			"that isn't bound yet. It's deleted on unmount, and exits by itself if the\n" +
			"connection drops. `tether tui` has a picker (K on a host).\n\n" +
			"Configure kubectl and the helper pod per host under [hosts.NAME.kube].",
	}
	cmd.AddCommand(newKubeLsCmd(g), newKubeMountCmd(g), newKubeUmountCmd(g), newKubeGCCmd(g))
	return cmd
}

func newKubeLsCmd(g *globalFlags) *cobra.Command {
	var p api.KubeListParams
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls HOST",
		Short: "List the claims kubectl on HOST can see, and whether they can be mounted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.Host = args[0]
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.KubeListResult
			if err := c.Call(cmd.Context(), api.MethodKubeList, p, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(res)
			}
			fmt.Fprintf(out, "context %s\n", res.Context)
			if len(res.PVCs) == 0 {
				fmt.Fprintln(out, "no claims")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "\nNAMESPACE\tNAME\tSTATUS\tSIZE\tCLASS\tMODES\tUSED BY\tMOUNT")
			for _, pvc := range res.PVCs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", pvc.Namespace, pvc.Name, pvc.Phase, dash(pvc.Size()),
					dash(pvc.StorageClass), dash(strings.Join(pvc.AccessModes, ",")), dash(usedBy(pvc)), mountability(pvc))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&p.Context, "context", "", "kubectl context (default: the current one)")
	cmd.Flags().StringVarP(&p.Namespace, "namespace", "n", "", "only this namespace (default: all you can list)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

// usedBy names the pods using a claim.
func usedBy(p api.PVC) string {
	var pods []string
	for _, u := range p.UsedBy {
		pods = append(pods, u.Pod)
	}
	return strings.Join(pods, ",")
}

// mountability is "yes", "yes: <what happens>" or "no: <why>".
func mountability(p api.PVC) string {
	switch {
	case !p.Mountable:
		return "no: " + p.Note
	case p.Note != "":
		return "yes: " + p.Note
	}
	return "yes"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newKubeMountCmd(g *globalFlags) *cobra.Command {
	var km api.KubeMount
	var options []string
	var wait waitFlags
	cmd := &cobra.Command{
		Use:   "mount HOST [CONTEXT/]NAMESPACE/CLAIM [DST]",
		Short: "Mount a claim here, running kubectl on HOST",
		Long: "Mount a claim here, running kubectl on HOST (connecting it if needed).\n" +
			"Without a context, kubectl's current one on HOST is used, and remembered.\n" +
			"Without DST, it's mounted at <mount_root>/<context>/<namespace>/<claim>\n" +
			"(mount_root defaults to ~/mnt/k8s).",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := ""
			if len(args) == 3 {
				dst = args[2]
			}
			if km.ReadOnly {
				options = append(options, "ro")
			}
			params, err := mountParams(args[0], mount.PVCPrefix+args[1], dst, options)
			if err != nil {
				return err
			}
			params.Kube.SubPath, params.Kube.ReadOnly = km.SubPath, km.ReadOnly
			return addMount(cmd, g, params, wait)
		},
	}
	cmd.Flags().BoolVar(&km.ReadOnly, "ro", false, "mount read-only (the helper pod too)")
	cmd.Flags().StringVar(&km.SubPath, "sub-path", "", "mount this directory inside the volume instead of its root")
	cmd.Flags().StringArrayVarP(&options, "option", "o", nil, "extra sshfs option (repeatable)")
	wait.register(cmd)
	return cmd
}

func newKubeUmountCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "umount HOST [CONTEXT/]NAMESPACE/CLAIM [DST]",
		Aliases: []string{"unmount", "rm"},
		Short:   "Unmount a claim and delete its helper pod",
		Args:    cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := ""
			if len(args) == 3 {
				dst = args[2]
			}
			params, err := mountParams(args[0], mount.PVCPrefix+args[1], dst, nil)
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

func newKubeGCCmd(g *globalFlags) *cobra.Command {
	var p api.KubeGCParams
	cmd := &cobra.Command{
		Use:   "gc HOST",
		Short: "Delete helper pods left behind by earlier daemons (yours, from this machine)",
		Long: "Delete helper pods left behind: finished ones, and running ones from an\n" +
			"earlier daemon on this machine (e.g. after a crash while the connection\n" +
			"stayed up). Pods of mounts that are up are left alone. Mounting cleans up\n" +
			"the claim's namespace by itself; this does every namespace.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.Host = args[0]
			c, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			var res api.KubeGCResult
			if err := c.Call(cmd.Context(), api.MethodKubeGC, p, &res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %d helper pods\n", res.Deleted)
			return nil
		},
	}
	cmd.Flags().StringVar(&p.Context, "context", "", "kubectl context (default: the current one)")
	return cmd
}

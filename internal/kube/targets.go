package kube

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
)

// Targets lists the services and pods kubectl in context kctx on m can
// forward to: in namespace ns, or in every namespace if ns is empty (falling
// back to the context's default namespace without permission for that).
func Targets(ctx context.Context, m *openssh.Master, opts Options, kctx, ns string) ([]api.KubeTarget, error) {
	scope := []string{"-A"}
	if ns != "" {
		scope = []string{"-n", ns}
	}
	var (
		wg             sync.WaitGroup
		svcOut, podOut []byte
		svcErr, podErr error
	)
	wg.Go(func() {
		svcOut, svcErr = run(ctx, m, opts, kctx, "", append([]string{"get", "svc", "-o", "json"}, scope...)...)
	})
	wg.Go(func() {
		podOut, podErr = run(ctx, m, opts, kctx, "", append([]string{"get", "pods", "-o", "json"}, scope...)...)
	})
	wg.Wait()
	if ns == "" && (svcErr != nil && isForbidden(svcErr) || podErr != nil && isForbidden(podErr)) {
		def, err := defaultNamespace(ctx, m, opts, kctx)
		if err == nil {
			return Targets(ctx, m, opts, kctx, def)
		}
	}
	if svcErr != nil && podErr != nil {
		return nil, svcErr
	}
	return parseTargets(svcOut, podOut)
}

type (
	portSpec struct {
		Name          string `json:"name"`
		Port          int    `json:"port"`          // services
		ContainerPort int    `json:"containerPort"` // pods
		Protocol      string `json:"protocol"`
	}
	svcObject struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Type  string     `json:"type"`
			Ports []portSpec `json:"ports"`
		} `json:"spec"`
	}
	targetPod struct {
		Metadata struct {
			objectMeta
			OwnerReferences []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Ports []portSpec `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
)

// parseTargets turns kubectl's JSON for services and pods into targets:
// services first, then running pods, each sorted by namespace and name.
// Only TCP ports are kept; kubectl can't forward UDP.
func parseTargets(svcJSON, podJSON []byte) ([]api.KubeTarget, error) {
	var out []api.KubeTarget
	if len(svcJSON) > 0 {
		var svcs list[svcObject]
		if err := json.Unmarshal(svcJSON, &svcs); err != nil {
			return nil, fmt.Errorf("reading kubectl output: %w", err)
		}
		for _, s := range svcs.Items {
			if s.Spec.Type == "ExternalName" {
				continue // no pods behind it to forward to
			}
			t := api.KubeTarget{Namespace: s.Metadata.Namespace, Kind: "svc", Name: s.Metadata.Name}
			for _, p := range s.Spec.Ports {
				if p.Protocol == "" || p.Protocol == "TCP" {
					t.Ports = append(t.Ports, api.KubePort{Name: p.Name, Port: p.Port})
				}
			}
			out = append(out, t)
		}
	}
	if len(podJSON) > 0 {
		var pods list[targetPod]
		if err := json.Unmarshal(podJSON, &pods); err != nil {
			return nil, fmt.Errorf("reading kubectl output: %w", err)
		}
		for _, p := range pods.Items {
			if p.Status.Phase != "Running" || p.Metadata.Labels[labelManagedBy] == managedBy {
				continue
			}
			t := api.KubeTarget{Namespace: p.Metadata.Namespace, Kind: "pod", Name: p.Metadata.Name}
			for _, o := range p.Metadata.OwnerReferences {
				t.Owner = o.Kind + "/" + o.Name
			}
			for _, c := range p.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Protocol == "" || cp.Protocol == "TCP" {
						t.Ports = append(t.Ports, api.KubePort{Name: cp.Name, Port: cp.ContainerPort})
					}
				}
			}
			out = append(out, t)
		}
	}
	kindOrder := map[string]int{"svc": 0, "pod": 1}
	slices.SortStableFunc(out, func(a, b api.KubeTarget) int {
		return cmp.Or(cmp.Compare(kindOrder[a.Kind], kindOrder[b.Kind]), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

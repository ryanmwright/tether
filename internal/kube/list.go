package kube

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
)

// PVC is a claim as listed for picking one to mount.
type PVC = api.PVC

// Consumer is a pod that uses a claim.
type Consumer = api.PVCConsumer

// The parts of Kubernetes objects we read.
type (
	objectMeta struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
	}
	pvcObject struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			AccessModes      []string `json:"accessModes"`
			StorageClassName *string  `json:"storageClassName"`
			VolumeName       string   `json:"volumeName"`
			Resources        struct {
				Requests map[string]string `json:"requests"`
			} `json:"resources"`
		} `json:"spec"`
		Status struct {
			Phase    string            `json:"phase"`
			Capacity map[string]string `json:"capacity"`
		} `json:"status"`
	}
	podObject struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			NodeName string `json:"nodeName"`
			Volumes  []struct {
				PersistentVolumeClaim *struct {
					ClaimName string `json:"claimName"`
				} `json:"persistentVolumeClaim"`
			} `json:"volumes"`
		} `json:"spec"`
		Status podStatus `json:"status"`
	}
	podStatus struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Ready bool `json:"ready"`
			State struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					Reason   string `json:"reason"`
					Message  string `json:"message"`
					ExitCode int    `json:"exitCode"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	}
	storageClassObject struct {
		Metadata          objectMeta `json:"metadata"`
		VolumeBindingMode string     `json:"volumeBindingMode"`
	}
	list[T any] struct {
		Items []T `json:"items"`
	}
)

// List lists the claims kubectl can see in context kctx (empty for the
// current one) on m: in namespace ns, or in every namespace if ns is empty.
// Without permission to list across namespaces, it falls back to the
// context's default namespace.
func List(ctx context.Context, m *openssh.Master, opts Options, kctx, ns string) ([]PVC, error) {
	scope := []string{"-A"}
	if ns != "" {
		scope = []string{"-n", ns}
	}
	var (
		wg                 sync.WaitGroup
		pvcOut, podOut, sc []byte
		pvcErr, podErr     error
	)
	wg.Go(func() {
		pvcOut, pvcErr = run(ctx, m, opts, kctx, "", append([]string{"get", "pvc", "-o", "json"}, scope...)...)
	})
	wg.Go(func() {
		podOut, podErr = run(ctx, m, opts, kctx, "", append([]string{"get", "pods", "-o", "json"}, scope...)...)
	})
	wg.Go(func() { sc, _ = run(ctx, m, opts, kctx, "", "get", "storageclass", "-o", "json") }) // optional: may be forbidden
	wg.Wait()
	if pvcErr != nil && ns == "" && isForbidden(pvcErr) {
		def, err := defaultNamespace(ctx, m, opts, kctx)
		if err != nil {
			return nil, pvcErr
		}
		return List(ctx, m, opts, kctx, def)
	}
	if pvcErr != nil {
		return nil, pvcErr
	}
	if podErr != nil && !isForbidden(podErr) {
		return nil, podErr
	}
	return parseList(pvcOut, podOut, sc)
}

func isForbidden(err error) bool {
	return strings.Contains(err.Error(), "Forbidden") || strings.Contains(err.Error(), "forbidden")
}

// defaultNamespace is the namespace kubectl uses in kctx without -n.
func defaultNamespace(ctx context.Context, m *openssh.Master, opts Options, kctx string) (string, error) {
	out, err := run(ctx, m, opts, kctx, "", "config", "view", "--minify", "-o", "jsonpath={..namespace}")
	if err != nil {
		return "", err
	}
	if ns := strings.TrimSpace(string(out)); ns != "" {
		return ns, nil
	}
	return "default", nil
}

// parseList turns kubectl's JSON for claims, pods and storage classes into
// classified PVCs, sorted by namespace and name. Pods and storage classes
// are optional.
func parseList(pvcJSON, podJSON, scJSON []byte) ([]PVC, error) {
	var pvcs list[pvcObject]
	if err := json.Unmarshal(pvcJSON, &pvcs); err != nil {
		return nil, fmt.Errorf("reading kubectl output: %w", err)
	}
	var pods list[podObject]
	if len(podJSON) > 0 {
		json.Unmarshal(podJSON, &pods)
	}
	var scs list[storageClassObject]
	if len(scJSON) > 0 {
		json.Unmarshal(scJSON, &scs)
	}
	binding := map[string]string{}
	for _, sc := range scs.Items {
		binding[sc.Metadata.Name] = sc.VolumeBindingMode
		if binding[sc.Metadata.Name] == "" {
			binding[sc.Metadata.Name] = "Immediate"
		}
	}
	users := consumers(pods.Items)

	out := make([]PVC, 0, len(pvcs.Items))
	for _, o := range pvcs.Items {
		p := PVC{
			Namespace:   o.Metadata.Namespace,
			Name:        o.Metadata.Name,
			Phase:       o.Status.Phase,
			Capacity:    o.Status.Capacity["storage"],
			Request:     o.Spec.Resources.Requests["storage"],
			AccessModes: abbreviate(o.Spec.AccessModes),
			Deleting:    o.Metadata.DeletionTimestamp != nil,
			UsedBy:      users[o.Metadata.Namespace+"/"+o.Metadata.Name],
		}
		if o.Spec.StorageClassName != nil {
			p.StorageClass = *o.Spec.StorageClassName
			p.BindingMode = binding[p.StorageClass]
		}
		classify(&p)
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b PVC) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// consumers maps "namespace/claim" to the pods still using it.
func consumers(pods []podObject) map[string][]Consumer {
	users := map[string][]Consumer{}
	for _, pod := range pods {
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue // finished pods don't hold volumes
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim == nil {
				continue
			}
			key := pod.Metadata.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
			users[key] = append(users[key], Consumer{
				Pod:    pod.Metadata.Name,
				Node:   pod.Spec.NodeName,
				Phase:  pod.Status.Phase,
				Tether: pod.Metadata.Labels[labelManagedBy] == managedBy,
			})
		}
	}
	return users
}

var modeAbbrev = map[string]string{
	"ReadWriteOnce":    "RWO",
	"ReadOnlyMany":     "ROX",
	"ReadWriteMany":    "RWX",
	"ReadWriteOncePod": "RWOP",
}

func abbreviate(modes []string) []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = cmp.Or(modeAbbrev[m], m)
	}
	return out
}

// classify decides whether p can be mounted and how.
func classify(p *PVC) {
	has := func(mode string) bool { return slices.Contains(p.AccessModes, mode) }
	p.Mountable, p.Note, p.Node = false, "", ""
	switch {
	case p.Deleting:
		p.Note = "being deleted"
		return
	case p.Phase == "Lost":
		p.Note = "lost its volume"
		return
	case p.Phase == "Pending" && p.BindingMode == "WaitForFirstConsumer":
		p.Mountable = true
		if len(p.UsedBy) == 0 {
			p.Note = "not bound yet; mounting provisions its volume"
		}
		return
	case p.Phase == "Pending" && p.BindingMode == "Immediate":
		p.Note = "pending: no volume bound yet"
		return
	case p.Phase == "Pending":
		p.Mountable = true
		p.Note = "pending; may not bind"
		return
	}
	// Bound.
	p.Mountable = true
	if len(p.UsedBy) == 0 {
		return
	}
	if has("RWOP") {
		p.Mountable = false
		p.Note = "in exclusive use (ReadWriteOncePod) by " + p.UsedBy[0].Pod
		return
	}
	if has("RWX") || has("ROX") {
		return
	}
	// ReadWriteOnce: only one node at a time, so join the pod using it.
	for _, u := range p.UsedBy {
		if u.Node != "" {
			p.Node = u.Node
			p.Note = "in use by " + u.Pod + "; helper runs on node " + u.Node
			return
		}
	}
}

// inspect fetches and classifies one claim, for mounting it.
func inspect(ctx context.Context, m *openssh.Master, s Source) (PVC, error) {
	pvcOut, err := run(ctx, m, s.Opts, s.Context, "", "get", "pvc", "-n", s.Namespace, s.PVC, "-o", "json")
	if err != nil {
		return PVC{}, err
	}
	var o pvcObject
	if err := json.Unmarshal(pvcOut, &o); err != nil {
		return PVC{}, fmt.Errorf("reading kubectl output: %w", err)
	}
	wrapped, _ := json.Marshal(list[pvcObject]{Items: []pvcObject{o}})
	podOut, _ := run(ctx, m, s.Opts, s.Context, "", "get", "pods", "-n", s.Namespace, "-o", "json")
	var scOut []byte
	if o.Spec.StorageClassName != nil && o.Status.Phase == "Pending" {
		one, err := run(ctx, m, s.Opts, s.Context, "", "get", "storageclass", *o.Spec.StorageClassName, "-o", "json")
		if err == nil {
			var sc storageClassObject
			json.Unmarshal(one, &sc)
			scOut, _ = json.Marshal(list[storageClassObject]{Items: []storageClassObject{sc}})
		}
	}
	pvcs, err := parseList(wrapped, podOut, scOut)
	if err != nil {
		return PVC{}, err
	}
	return pvcs[0], nil
}

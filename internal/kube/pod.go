package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ryanmwright/tether/internal/linelog"
	"github.com/ryanmwright/tether/internal/openssh"
)

// DataPath is where helper pods mount the claim.
const DataPath = "/data"

const (
	containerName = "sftp"
	pollInterval  = time.Second
	stopTimeout   = 10 * time.Second
)

// Pod is a running helper pod that mounts a claim. It lives as long as its
// attach session: tether holds `kubectl attach -i` open, and the pod's only
// process exits when that stdin closes. If the daemon, the connection or
// kubectl dies, the pod finishes on its own instead of lingering.
type Pod struct {
	Name, Namespace, Node string

	m      *openssh.Master
	src    Source
	attach *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	once   sync.Once
	err    error
}

// Done is closed when the pod goes away (its attach session ended).
func (p *Pod) Done() <-chan struct{} { return p.done }

// Err says why the pod went away. Only valid after Done is closed.
func (p *Pod) Err() error { return p.err }

// SFTPCommand runs the image's sftp-server in the pod, speaking SFTP on its
// stdin and stdout, starting in DataPath: mount "." from it.
func (p *Pod) SFTPCommand() *exec.Cmd {
	return p.m.Command(nil, kubectl(p.src.Opts, p.src.Context, "exec", "-i", "-n", p.Namespace, p.Name, "-c", containerName, "--",
		p.src.Opts.WithDefaults().SFTPServer, "-d", DataPath))
}

// Stop ends the attach session and deletes the pod.
func (p *Pod) Stop(ctx context.Context) error {
	p.stdin.Close() // the pod's process sees EOF and exits
	select {
	case <-p.done:
	case <-time.After(stopTimeout):
		p.attach.Process.Kill()
		<-p.done
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	_, err := run(ctx, p.m, p.src.Opts, p.src.Context, "", "delete", "pod", "-n", p.Namespace, p.Name, "--wait=false", "--ignore-not-found")
	return err
}

// StartPod starts a helper pod for s over m and waits until it's ready. An
// empty s.Context means kubectl's current one.
func StartPod(ctx context.Context, m *openssh.Master, s Source, log *slog.Logger) (*Pod, error) {
	if s.Context == "" {
		c, err := CurrentContext(ctx, m, s.Opts)
		if err != nil {
			return nil, err
		}
		s.Context = c
	}
	opts := s.Opts.WithDefaults()
	pvc, err := inspect(ctx, m, s)
	if err != nil {
		return nil, err
	}
	if !pvc.Mountable {
		return nil, fmt.Errorf("can't mount %s: %s", s.Ref(), pvc.Note)
	}
	gc(ctx, m, s.Opts, s.Context, s.Namespace)

	name := podName(s.PVC)
	manifest, err := json.Marshal(podManifest(name, s, pvc.Node))
	if err != nil {
		return nil, err
	}
	if _, err := run(ctx, m, s.Opts, s.Context, string(manifest), "create", "-f", "-"); err != nil {
		return nil, fmt.Errorf("creating helper pod: %w", err)
	}
	log = log.With("pod", s.Namespace+"/"+name)
	log.Info("helper pod created", "node", pvc.Node)
	del := func() {
		dctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		run(dctx, m, s.Opts, s.Context, "", "delete", "pod", "-n", s.Namespace, name, "--wait=false", "--ignore-not-found")
	}

	wctx, cancel := context.WithTimeout(ctx, opts.StartTimeout)
	defer cancel()
	node, err := waitReady(wctx, m, s, name)
	if err != nil {
		del()
		return nil, err
	}

	p := &Pod{Name: name, Namespace: s.Namespace, Node: node, m: m, src: s, done: make(chan struct{})}
	p.attach = m.Command(nil, kubectl(s.Opts, s.Context, "attach", "-i", "-q", "-n", s.Namespace, name, "-c", containerName))
	attachErr := linelog.New(log, "kubectl attach output")
	p.attach.Stderr = attachErr
	if p.stdin, err = p.attach.StdinPipe(); err != nil {
		del()
		return nil, err
	}
	if err := p.attach.Start(); err != nil {
		del()
		return nil, err
	}
	go func() {
		err := p.attach.Wait()
		p.once.Do(func() {
			msg := attachErr.Last()
			switch {
			case msg != "":
				p.err = fmt.Errorf("helper pod %s: %s", name, msg)
			case err != nil:
				p.err = fmt.Errorf("helper pod %s: attach exited: %w", name, err)
			default:
				p.err = fmt.Errorf("helper pod %s exited", name)
			}
			close(p.done)
		})
	}()
	return p, nil
}

// podName is "tether-<claim>-<random>", within the 63 characters a pod's
// hostname allows.
func podName(pvc string) string {
	base := strings.Trim(labelBad.ReplaceAllString(strings.ReplaceAll(pvc, ".", "-"), "-"), "-")
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "-")
	}
	return "tether-" + base + "-" + randomSuffix(5)
}

// podManifest describes the helper pod. It holds the claim at DataPath and
// runs `cat` on a stdin that closes when tether's attach session ends.
func podManifest(name string, s Source, node string) map[string]any {
	opts := s.Opts.WithDefaults()
	mnt := map[string]any{"name": "data", "mountPath": DataPath}
	if s.ReadOnly {
		mnt["readOnly"] = true
	}
	if s.SubPath != "" {
		mnt["subPath"] = s.SubPath
	}
	claim := map[string]any{"claimName": s.PVC}
	if s.ReadOnly {
		claim["readOnly"] = true
	}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"automountServiceAccountToken":  false,
		"terminationGracePeriodSeconds": 1,
		"enableServiceLinks":            false,
		"containers": []any{map[string]any{
			"name":            containerName,
			"image":           opts.Image,
			"imagePullPolicy": "IfNotPresent",
			"command":         []string{"sh", "-c", "cat >/dev/null"},
			"stdin":           true,
			"stdinOnce":       true,
			"volumeMounts":    []any{mnt},
			"resources": map[string]any{
				"requests": map[string]string{"cpu": "10m", "memory": "16Mi"},
			},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"seccompProfile":           map[string]string{"type": "RuntimeDefault"},
			},
		}},
		"volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": claim}},
	}
	psc := map[string]any{}
	if opts.RunAsUser != nil {
		psc["runAsUser"] = *opts.RunAsUser
		psc["runAsNonRoot"] = *opts.RunAsUser != 0
	}
	if opts.RunAsGroup != nil {
		psc["runAsGroup"] = *opts.RunAsGroup
	}
	if opts.FSGroup != nil {
		psc["fsGroup"] = *opts.FSGroup
	}
	if len(psc) > 0 {
		spec["securityContext"] = psc
	}
	if node != "" {
		// A ReadWriteOnce volume in use can only be mounted on its node.
		spec["affinity"] = map[string]any{"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{map[string]any{
					"matchFields": []any{map[string]any{"key": "metadata.name", "operator": "In", "values": []string{node}}},
				}},
			},
		}}
		spec["tolerations"] = []any{map[string]string{"operator": "Exists"}}
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": s.Namespace,
			"labels": map[string]string{
				labelManagedBy: managedBy,
				labelOwner:     Owner,
				labelInstance:  instance,
			},
			"annotations": map[string]string{"tether.dev/pvc": s.PVC},
		},
		"spec": spec,
	}
}

// Failures that won't fix themselves while we wait.
var fatalWaiting = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "CrashLoopBackOff": true,
	"RunContainerError": true,
}

// waitReady polls the pod until it's ready, failing early on errors that
// won't go away and explaining a timeout with the pod's conditions and
// events. It returns the pod's node.
func waitReady(ctx context.Context, m *openssh.Master, s Source, name string) (string, error) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	var last podObject
	for {
		out, err := run(ctx, m, s.Opts, s.Context, "", "get", "pod", "-n", s.Namespace, name, "-o", "json")
		if err == nil {
			var pod podObject
			if json.Unmarshal(out, &pod) == nil {
				last = pod
				if ready, err := podReady(pod); ready || err != nil {
					return pod.Spec.NodeName, err
				}
			}
		}
		select {
		case <-ctx.Done():
			// Use a fresh context: ctx has run out.
			ectx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			why := explain(ectx, m, s, name, last)
			if why == "" {
				why = "not ready"
			}
			return "", fmt.Errorf("helper pod %s: %s (after %s)", name, why, s.Opts.WithDefaults().StartTimeout)
		case <-tick.C:
		}
	}
}

func podReady(pod podObject) (bool, error) {
	switch pod.Status.Phase {
	case "Succeeded", "Failed":
		return false, fmt.Errorf("helper pod exited (%s)", pod.Status.Phase)
	}
	for _, c := range pod.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil && fatalWaiting[w.Reason] {
			return false, fmt.Errorf("helper pod: %s: %s", w.Reason, w.Message)
		}
		if t := c.State.Terminated; t != nil {
			return false, fmt.Errorf("helper pod exited: %s %s", t.Reason, t.Message)
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true, nil
		}
	}
	return false, nil
}

// explain says why a pod isn't ready: its latest warning event (e.g. a
// multi-attach error), or a condition that isn't met (e.g. unschedulable).
func explain(ctx context.Context, m *openssh.Master, s Source, name string, pod podObject) string {
	out, err := run(ctx, m, s.Opts, s.Context, "", "get", "events", "-n", s.Namespace,
		"--field-selector", "involvedObject.name="+name+",type=Warning", "-o", "json")
	if err == nil {
		var events list[struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}]
		if json.Unmarshal(out, &events) == nil && len(events.Items) > 0 {
			e := events.Items[len(events.Items)-1]
			return e.Reason + ": " + e.Message
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Status != "True" && c.Message != "" {
			return c.Reason + ": " + c.Message
		}
	}
	for _, c := range pod.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil {
			return strings.TrimSpace(w.Reason + ": " + w.Message)
		}
	}
	if pod.Status.Phase != "" {
		return "still " + pod.Status.Phase
	}
	return ""
}

// gc deletes this user's helper pods in namespace ns (every namespace if
// empty) that have finished, or that a previous daemon left running. It
// returns how many it deleted.
func gc(ctx context.Context, m *openssh.Master, opts Options, kctx, ns string) (int, error) {
	scope := []string{"-A"}
	if ns != "" {
		scope = []string{"-n", ns}
	}
	mine := labelManagedBy + "=" + managedBy + "," + labelOwner + "=" + Owner
	var deleted int
	// Finished pods, from any daemon of this user here.
	finished := append([]string{"delete", "pods", "-l", mine, "--field-selector", "status.phase!=Running,status.phase!=Pending", "--wait=false", "--ignore-not-found", "-o", "name"}, scope...)
	out, err1 := run(ctx, m, opts, kctx, "", finished...)
	deleted += countLines(out)
	// Pods from earlier daemons, still running (e.g. the connection died
	// before the attach session started).
	stale := append([]string{"delete", "pods", "-l", mine + "," + labelInstance + "!=" + instance, "--wait=false", "--ignore-not-found", "-o", "name"}, scope...)
	out, err2 := run(ctx, m, opts, kctx, "", stale...)
	deleted += countLines(out)
	return deleted, errors.Join(err1, err2)
}

// GC deletes leftover helper pods of this user in context kctx, in every
// namespace.
func GC(ctx context.Context, m *openssh.Master, opts Options, kctx string) (int, error) {
	return gc(ctx, m, opts, kctx, "")
}

func countLines(b []byte) int {
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/mount"
	"github.com/ryanmwright/tether/internal/openssh"
)

const mountStartTimeout = 30 * time.Second

type wantMount struct {
	spec     mount.Spec
	profiles []string
	adhoc    bool
}

type mountState struct {
	state     api.State
	err       string
	pod, node string // a PVC mount's helper pod
}

// startTimeout is how long mounting spec may take: a PVC mount waits for a
// helper pod, which may have to pull its image and attach the volume.
func startTimeout(spec mount.Spec) time.Duration {
	if spec.Kube != nil {
		return spec.Kube.Opts.WithDefaults().StartTimeout + mountStartTimeout
	}
	return mountStartTimeout
}

// syncMounts unmounts what's no longer wanted and mounts what's missing. It
// reports whether any mount failed, so the caller can schedule a retry.
func (s *session) syncMounts(ctx context.Context, m *openssh.Master, w want, running map[string]*mount.Running) (failed bool) {
	for _, key := range slices.Sorted(maps.Keys(running)) {
		if _, ok := w.mounts[key]; !ok {
			s.stopMount(key, running)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(w.mounts)) {
		if running[key] != nil {
			continue
		}
		spec := w.mounts[key].spec
		sctx, cancel := context.WithTimeout(ctx, startTimeout(spec))
		r, err := mount.Start(sctx, m, spec, s.log.With("mount", key))
		cancel()
		if prev := s.setMount(key, r, err); err != nil {
			if prev != err.Error() {
				s.log.Warn("mount failed", "mount", key, "err", err)
			}
			failed = true
			continue
		}
		s.log.Info("mounted", "mount", key)
		running[key] = r
		go func() {
			<-r.Done()
			poke(s.mountDied)
		}()
	}
	return failed
}

// reapMounts notices mounts that went away on their own (unmounted by hand,
// sshfs crashed) and marks them failed so they're retried.
func (s *session) reapMounts(running map[string]*mount.Running) (failed bool) {
	for key, r := range running {
		select {
		case <-r.Done():
			delete(running, key)
			err := fmt.Errorf("mount went away: %w", r.Err())
			s.log.Warn("mount lost", "mount", key, "err", r.Err())
			s.setMount(key, nil, err)
			failed = true
		default:
		}
	}
	return failed
}

// stopMounts unmounts everything, e.g. before disconnecting.
func (s *session) stopMounts(running map[string]*mount.Running) {
	for _, key := range slices.Sorted(maps.Keys(running)) {
		s.stopMount(key, running)
	}
}

func (s *session) stopMount(key string, running map[string]*mount.Running) {
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	if err := running[key].Stop(ctx); err != nil {
		s.log.Warn("unmount had problems", "mount", key, "err", err)
	} else {
		s.log.Info("unmounted", "mount", key)
	}
	delete(running, key)
}

// setMount records the outcome of mounting and returns the previous error.
func (s *session) setMount(key string, r *mount.Running, err error) (prevErr string) {
	s.mu.Lock()
	if ms := s.mounts[key]; ms != nil {
		prevErr = ms.err
		if err != nil {
			*ms = mountState{state: api.StateError, err: err.Error()}
		} else {
			*ms = mountState{state: api.StateUp, pod: r.Pod, node: r.Node}
		}
	}
	s.mu.Unlock()
	s.changed()
	return prevErr
}

// mountStatuses reports mounts for status. Callers hold s.mu.
func (s *session) mountStatuses() []api.MountStatus {
	out := []api.MountStatus{}
	for _, key := range slices.Sorted(maps.Keys(s.mounts)) {
		ms, wm := s.mounts[key], s.want.mounts[key]
		st := api.MountStatus{
			Key:       key,
			Direction: string(wm.spec.Direction),
			Remote:    wm.spec.Remote,
			Local:     wm.spec.Local,
			Profiles:  wm.profiles,
			AdHoc:     wm.adhoc,
			State:     ms.state,
			Error:     ms.err,
		}
		if k := wm.spec.Kube; k != nil {
			st.Kube = &api.KubeMountStatus{
				KubeMount: api.KubeMount{Context: k.Context, Namespace: k.Namespace, PVC: k.PVC, SubPath: k.SubPath, ReadOnly: k.ReadOnly},
				Pod:       ms.pod,
				Node:      ms.node,
			}
		}
		out = append(out, st)
	}
	return out
}

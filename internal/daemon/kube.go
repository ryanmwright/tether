package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/config"
	"github.com/ryanmwright/tether/internal/kube"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/rpc"
)

const kubeTimeout = 60 * time.Second

var tmpConns atomic.Uint64

// kubeMaster returns a connection to run kubectl on a host: its live one,
// a local one for the local host, or else a temporary connection that
// release closes.
func (d *Daemon) kubeMaster(ctx context.Context, name string) (m *openssh.Master, host config.Host, release func(), err error) {
	d.mu.Lock()
	host, ok := d.host(name)
	h := d.sessions[name]
	d.mu.Unlock()
	if !ok || h == nil {
		return nil, host, nil, rpc.Errorf(api.CodeNotFound, "no host named %q", name)
	}
	if host.Local {
		return openssh.StartLocal(d.log), host, func() {}, nil
	}
	if m := h.s.liveMaster(); m != nil {
		return m, host, func() {}, nil
	}
	ctl := filepath.Join(filepath.Dir(d.opts.SocketPath), "tmp", fmt.Sprintf("%s.%d", name, tmpConns.Add(1)))
	os.MkdirAll(filepath.Dir(ctl), 0o700)
	m, err = openssh.Start(ctx, d.opts.SSH, host.SSH, ctl, d.log.With("host", name, "temporary", true))
	if err != nil {
		return nil, host, nil, fmt.Errorf("connecting to %s: %w", name, err)
	}
	return m, host, m.Stop, nil
}

func (d *Daemon) handleKubeList(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.KubeListParams](params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, kubeTimeout)
	defer cancel()
	m, host, release, err := d.kubeMaster(ctx, p.Host)
	if err != nil {
		return nil, err
	}
	defer release()
	opts := host.Kube.Options()

	contexts, current, err := kube.Contexts(ctx, m, opts)
	if err != nil {
		return nil, err
	}
	kctx := p.Context
	if kctx == "" {
		kctx = current
	}
	if kctx == "" {
		return nil, fmt.Errorf("kubectl on %s has no current context; pick one of: %s", p.Host, strings.Join(contexts, ", "))
	}
	pvcs, err := kube.List(ctx, m, opts, kctx, p.Namespace)
	if err != nil {
		return nil, err
	}
	root := opts.MountRoot
	if rest, ok := strings.CutPrefix(root, "~/"); ok {
		root = filepath.Join(d.home, rest)
	}
	return api.KubeListResult{Host: p.Host, Context: kctx, Current: current, Contexts: contexts, PVCs: pvcs, MountRoot: root}, nil
}

func (d *Daemon) handleKubeGC(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.KubeGCParams](params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, kubeTimeout)
	defer cancel()
	m, host, release, err := d.kubeMaster(ctx, p.Host)
	if err != nil {
		return nil, err
	}
	defer release()
	opts := host.Kube.Options()
	kctx := p.Context
	if kctx == "" {
		if kctx, err = kube.CurrentContext(ctx, m, opts); err != nil {
			return nil, err
		}
	}
	n, err := kube.GC(ctx, m, opts, kctx)
	if err != nil {
		return nil, err
	}
	d.log.Info("deleted leftover helper pods", "host", p.Host, "context", kctx, "count", n)
	return api.KubeGCResult{Deleted: n}, nil
}

// maxRecent is how many claims are remembered per host.
const maxRecent = 5

// rememberPVC records a claim just mounted from host. Callers hold d.mu.
func (d *Daemon) rememberPVC(host string, r api.RecentPVC) {
	list := slices.DeleteFunc(slices.Clone(d.recent[host]), func(o api.RecentPVC) bool {
		return o.Context == r.Context && o.Namespace == r.Namespace && o.PVC == r.PVC
	})
	list = append([]api.RecentPVC{r}, list...)
	d.recent[host] = list[:min(len(list), maxRecent)]
	if d.opts.RecentFile == "" {
		return
	}
	b, _ := json.MarshalIndent(d.recent, "", "  ")
	err := os.MkdirAll(filepath.Dir(d.opts.RecentFile), 0o700)
	if err == nil {
		err = os.WriteFile(d.opts.RecentFile, b, 0o600)
	}
	if err != nil {
		d.log.Warn("couldn't save recent claims", "err", err)
	}
}

// loadRecent reads the recent claims kept by rememberPVC.
func loadRecent(path string) map[string][]api.RecentPVC {
	recent := map[string][]api.RecentPVC{}
	if path == "" {
		return recent
	}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &recent)
	}
	return recent
}

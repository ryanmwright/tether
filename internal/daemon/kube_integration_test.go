package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/kubetest"
)

func needFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("sshfs"); err != nil {
		t.Skip("sshfs not found")
	}
}

// TestPVCMountLocal mounts a claim on the built-in local host: kubectl runs
// here, as when tether runs on the jump box itself.
func TestPVCMountLocal(t *testing.T) {
	needFUSE(t)
	fake := kubetest.Install(t)
	vol := fake.AddPVC(t, "db", "data")
	os.WriteFile(filepath.Join(vol, "f.txt"), []byte("in the volume"), 0o644)
	mp := mountDirs(t, "mp")[0]
	t.Setenv("TETHER_TEST_LOCAL_HOST", "1")
	hh := start(t, "")
	h := &sshHarness{harness: hh, c: hh.client(t)}

	st := h.status(t)
	if l := host(st, "local"); !l.Local || l.State != api.StateDown {
		t.Fatalf("local host = %+v", l)
	}

	var list api.KubeListResult
	h.call(t, api.MethodKubeList, api.KubeListParams{Host: "local"}, &list)
	if list.Context != kubetest.Context || len(list.PVCs) != 1 || list.PVCs[0].Ref() != "db/data" || !list.PVCs[0].Mountable {
		t.Fatalf("kube.list = %+v", list)
	}
	if !strings.HasSuffix(list.MountRoot, "/mnt/k8s") {
		t.Errorf("mount root = %q", list.MountRoot)
	}

	params := api.MountParams{Host: "local", Direction: "pvc-to-local", Local: mp, Kube: &api.KubeMount{Namespace: "db", PVC: "data"}}
	var res api.MountResult
	h.call(t, api.MethodMountAdd, params, &res)
	if want := "pvc:" + kubetest.Context + "/db/data -> " + mp; res.Key != want {
		t.Errorf("key = %q, want %q (context resolved when added)", res.Key, want)
	}
	st = h.waitFor(t, "mount up", func(st api.Status) bool { return mountOf(st, "local", res.Key).State == api.StateUp })
	m := mountOf(st, "local", res.Key)
	if m.Kube == nil || m.Kube.Pod == "" || m.Kube.Node != kubetest.Node || m.Direction != "pvc-to-local" {
		t.Errorf("mount status = %+v kube=%+v", m, m.Kube)
	}
	if readFile(filepath.Join(mp, "f.txt")) != "in the volume" {
		t.Fatal("volume not visible through the mount")
	}
	if r := host(st, "local").RecentPVCs; len(r) != 1 || r[0].Context != kubetest.Context || r[0].PVC != "data" || r[0].Local != mp {
		t.Errorf("recent claims = %+v", r)
	}
	os.WriteFile(filepath.Join(mp, "new.txt"), []byte("written here"), 0o644)
	if readFile(filepath.Join(vol, "new.txt")) != "written here" {
		t.Error("write through the mount didn't reach the volume")
	}

	// The pod going away takes the mount with it, and a new pod replaces it.
	first := m.Kube.Pod
	fake.KillPod(first)
	h.waitFor(t, "mount lost", func(st api.Status) bool { return mountOf(st, "local", res.Key).State == api.StateError })
	h.call(t, api.MethodUp, api.TargetParams{Name: "local"}, nil) // retry now
	h.waitFor(t, "mount back", func(st api.Status) bool {
		m := mountOf(st, "local", res.Key)
		return m.State == api.StateUp && m.Kube.Pod != first
	})
	if readFile(filepath.Join(mp, "f.txt")) != "in the volume" {
		t.Fatal("remount not working")
	}

	// Removing it without naming the context finds it; the pod is deleted.
	params.Kube.Context = ""
	h.call(t, api.MethodMountRemove, params, nil)
	// Status drops the mount at once; unmounting follows.
	h.waitFor(t, "unmounted and pod deleted", func(api.Status) bool {
		return readFile(filepath.Join(mp, "f.txt")) == "" && len(fake.Pods()) == 0
	})

	// The local host has nothing but claims.
	if err := h.c.Call(t.Context(), api.MethodForwardAdd, api.ForwardParams{Host: "local", Spec: "L:1234:x:1"}, nil); err == nil {
		t.Error("forward accepted on the local host")
	}
}

// TestPVCMountViaSSH mounts a claim here with kubectl running on the remote,
// as through a jump box.
func TestPVCMountViaSSH(t *testing.T) {
	needFUSE(t)
	fake := kubetest.Install(t) // before starting sshd, so remote sessions get it on PATH
	vol := fake.AddPVC(t, "apps", "cache")
	os.WriteFile(filepath.Join(vol, "c.txt"), []byte("cached"), 0o644)
	mp := mountDirs(t, "mp")[0]
	h := startWithMounts(t, `
[hosts.dev.kube]
start_timeout = "20s"

[profiles.k8s]
host = "dev"
[[profiles.k8s.mounts]]
pvc = "`+kubetest.Context+`/apps/cache"
local = "`+mp+`"
read_only = true
`)

	var list api.KubeListResult
	h.call(t, api.MethodKubeList, api.KubeListParams{Host: "dev"}, &list) // over a temporary connection
	if len(list.PVCs) != 1 || list.PVCs[0].Ref() != "apps/cache" {
		t.Fatalf("kube.list = %+v", list)
	}
	if host(h.status(t), "dev").State != api.StateDown {
		t.Error("listing left the host connected")
	}

	var dr api.DoctorResult
	h.call(t, api.MethodDoctor, api.DoctorParams{Host: "dev"}, &dr)
	for _, name := range []string{"kubectl", "kube access"} {
		found := false
		for _, c := range dr.Checks {
			if c.Name == name {
				found = true
				if c.Status != api.CheckOK {
					t.Errorf("doctor %s = %+v", name, c)
				}
			}
		}
		if !found {
			t.Errorf("doctor has no %s check", name)
		}
	}

	h.call(t, api.MethodUp, api.TargetParams{Name: "k8s"}, nil)
	st := h.waitFor(t, "profile up", func(st api.Status) bool { return profile(st, "k8s").State == api.StateUp })
	key := "pvc:" + kubetest.Context + "/apps/cache -> " + mp
	if m := mountOf(st, "dev", key); m.State != api.StateUp || m.Kube == nil || !m.Kube.ReadOnly {
		t.Errorf("mount = %+v", m)
	}
	if readFile(filepath.Join(mp, "c.txt")) != "cached" {
		t.Fatal("volume not visible through the mount")
	}
	if !strings.Contains(fake.Calls(), "exec -i -n apps tether-cache-") || !strings.Contains(fake.Calls(), "touch /tether/alive") {
		t.Errorf("no heartbeat session for the helper pod; calls:\n%s", fake.Calls())
	}

	// Losing the connection ends the pod (its heartbeats stop) and
	// the mount; both come back with the connection.
	pod := mountOf(st, "dev", key).Kube.Pod
	h.srv.Stop()
	h.waitFor(t, "connection lost", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	h.srv.Restart(t)
	h.waitFor(t, "profile back up", func(st api.Status) bool {
		m := mountOf(st, "dev", key)
		return profile(st, "k8s").State == api.StateUp && m.Kube != nil && m.Kube.Pod != pod
	})
	if readFile(filepath.Join(mp, "c.txt")) != "cached" {
		t.Fatal("mount not restored after reconnect")
	}

	h.call(t, api.MethodDown, api.TargetParams{Name: "k8s"}, nil)
	h.waitFor(t, "host down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })
	if readFile(filepath.Join(mp, "c.txt")) != "" {
		t.Error("still mounted after down")
	}
}

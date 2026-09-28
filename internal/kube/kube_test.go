package kube

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	for _, tc := range []struct {
		in, ctx, ns, pvc string
		bad              bool
	}{
		{in: "db/data-pg-0", ns: "db", pvc: "data-pg-0"},
		{in: "prod/db/data", ctx: "prod", ns: "db", pvc: "data"},
		{in: "arn:aws:eks:us-east-1:123:cluster/prod/db/data", ctx: "arn:aws:eks:us-east-1:123:cluster/prod", ns: "db", pvc: "data"},
		{in: "data", bad: true},
		{in: "db/Data", bad: true},
		{in: "-x/db/data", bad: true},
		{in: "Db/data", bad: true},
	} {
		s, err := ParseRef(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseRef(%q) = %+v, want error", tc.in, s)
			}
			continue
		}
		if err != nil || s.Context != tc.ctx || s.Namespace != tc.ns || s.PVC != tc.pvc {
			t.Errorf("ParseRef(%q) = %+v, %v", tc.in, s, err)
		}
	}
	if err := (Source{Namespace: "a", PVC: "b", SubPath: "x/../../etc"}).Validate(); err == nil {
		t.Error("sub-path with .. accepted")
	}
	if err := (Source{Namespace: "a", PVC: "b", SubPath: "/abs"}).Validate(); err == nil {
		t.Error("absolute sub-path accepted")
	}
}

func TestKubectlCommand(t *testing.T) {
	got := kubectl(Options{Kubectl: "~/bin/kubectl", Kubeconfig: "/etc/k 1"}, "it's", "get", "pvc")
	want := `"$HOME"/'bin/kubectl' --kubeconfig '/etc/k 1' --context 'it'"'"'s' 'get' 'pvc'`
	if got != want {
		t.Errorf("kubectl = %s\nwant      %s", got, want)
	}
	if got := kubectl(Options{}, "", "version"); got != `'kubectl' 'version'` {
		t.Errorf("default kubectl = %s", got)
	}
}

const pvcsJSON = `{"items":[
 {"metadata":{"namespace":"db","name":"bound-free"},"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"fast"},"status":{"phase":"Bound","capacity":{"storage":"10Gi"}}},
 {"metadata":{"namespace":"db","name":"bound-rwo-used"},"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"fast"},"status":{"phase":"Bound"}},
 {"metadata":{"namespace":"db","name":"bound-rwx-used"},"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"nfs"},"status":{"phase":"Bound"}},
 {"metadata":{"namespace":"db","name":"rwop-used"},"spec":{"accessModes":["ReadWriteOncePod"],"storageClassName":"fast"},"status":{"phase":"Bound"}},
 {"metadata":{"namespace":"apps","name":"wffc"},"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"local","resources":{"requests":{"storage":"5Gi"}}},"status":{"phase":"Pending"}},
 {"metadata":{"namespace":"apps","name":"immediate"},"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"fast"},"status":{"phase":"Pending"}},
 {"metadata":{"namespace":"apps","name":"lost"},"spec":{"accessModes":["ReadWriteOnce"]},"status":{"phase":"Lost"}},
 {"metadata":{"namespace":"apps","name":"going","deletionTimestamp":"2026-01-01T00:00:00Z"},"spec":{},"status":{"phase":"Bound"}}
]}`

const podsJSON = `{"items":[
 {"metadata":{"namespace":"db","name":"pg-0"},"spec":{"nodeName":"n2","volumes":[{"persistentVolumeClaim":{"claimName":"bound-rwo-used"}}]},"status":{"phase":"Running"}},
 {"metadata":{"namespace":"db","name":"web-1"},"spec":{"nodeName":"n3","volumes":[{"persistentVolumeClaim":{"claimName":"bound-rwx-used"}}]},"status":{"phase":"Running"}},
 {"metadata":{"namespace":"db","name":"solo"},"spec":{"nodeName":"n1","volumes":[{"persistentVolumeClaim":{"claimName":"rwop-used"}}]},"status":{"phase":"Running"}},
 {"metadata":{"namespace":"db","name":"old-job"},"spec":{"nodeName":"n1","volumes":[{"persistentVolumeClaim":{"claimName":"bound-free"}}]},"status":{"phase":"Succeeded"}}
]}`

const scJSON = `{"items":[
 {"metadata":{"name":"fast"}},
 {"metadata":{"name":"local"},"volumeBindingMode":"WaitForFirstConsumer"}
]}`

func TestClassify(t *testing.T) {
	pvcs, err := parseList([]byte(pvcsJSON), []byte(podsJSON), []byte(scJSON))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]PVC{}
	for _, p := range pvcs {
		byName[p.Name] = p
	}
	if pvcs[0].Namespace != "apps" {
		t.Errorf("not sorted by namespace: %v", pvcs[0].Ref())
	}
	for _, tc := range []struct {
		name      string
		mountable bool
		node      string
		note      string
	}{
		{"bound-free", true, "", ""}, // the finished job doesn't count
		{"bound-rwo-used", true, "n2", "pg-0"},
		{"bound-rwx-used", true, "", ""},
		{"rwop-used", false, "", "exclusive"},
		{"wffc", true, "", "provisions"},
		{"immediate", false, "", "no volume"},
		{"lost", false, "", "lost"},
		{"going", false, "", "deleted"},
	} {
		p := byName[tc.name]
		if p.Mountable != tc.mountable || p.Node != tc.node || !strings.Contains(p.Note, tc.note) {
			t.Errorf("%s: mountable=%v node=%q note=%q", tc.name, p.Mountable, p.Node, p.Note)
		}
	}
	if p := byName["wffc"]; p.Size() != "5Gi" || p.BindingMode != "WaitForFirstConsumer" {
		t.Errorf("wffc = %+v", p)
	}
	if p := byName["bound-free"]; p.Size() != "10Gi" || strings.Join(p.AccessModes, ",") != "RWO" {
		t.Errorf("bound-free = %+v", p)
	}
	// Without storage classes (forbidden), a pending claim may still work.
	pvcs, _ = parseList([]byte(pvcsJSON), nil, nil)
	for _, p := range pvcs {
		if p.Name == "immediate" && !p.Mountable {
			t.Errorf("pending claim with unknown binding mode not mountable: %+v", p)
		}
	}
}

func TestPodManifest(t *testing.T) {
	uid := int64(999)
	s := Source{Namespace: "db", PVC: "data", SubPath: "pgdata", ReadOnly: true, Opts: Options{RunAsUser: &uid, Image: "reg/sftp:1"}}
	b, _ := json.Marshal(podManifest("tether-data-abcde", s, "n2"))
	var pod struct {
		Metadata struct {
			Name, Namespace string
			Labels          map[string]string
		}
		Spec struct {
			Affinity        map[string]any
			Tolerations     []map[string]string
			SecurityContext map[string]any
			Containers      []struct {
				Image, Name  string
				VolumeMounts []map[string]any
			}
			Volumes []struct {
				PersistentVolumeClaim map[string]any
			}
		}
	}
	if err := json.Unmarshal(b, &pod); err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0]
	switch {
	case pod.Metadata.Namespace != "db" || pod.Metadata.Labels[labelOwner] != Owner || pod.Metadata.Labels[labelInstance] != instance:
		t.Errorf("metadata = %+v", pod.Metadata)
	case c.Image != "reg/sftp:1" || len(c.VolumeMounts) != 2:
		t.Errorf("container = %+v", c)
	case c.VolumeMounts[0]["subPath"] != "pgdata" || c.VolumeMounts[0]["readOnly"] != true || c.VolumeMounts[0]["mountPath"] != DataPath:
		t.Errorf("volume mount = %+v", c.VolumeMounts)
	case pod.Spec.Volumes[0].PersistentVolumeClaim["claimName"] != "data" || pod.Spec.Volumes[0].PersistentVolumeClaim["readOnly"] != true:
		t.Errorf("volume = %+v", pod.Spec.Volumes)
	case pod.Spec.SecurityContext["runAsUser"] != float64(999) || pod.Spec.SecurityContext["runAsNonRoot"] != true:
		t.Errorf("securityContext = %+v", pod.Spec.SecurityContext)
	case pod.Spec.Affinity == nil || len(pod.Spec.Tolerations) != 1:
		t.Errorf("not pinned to the node: %+v %+v", pod.Spec.Affinity, pod.Spec.Tolerations)
	}
	if !strings.Contains(string(b), `"values":["n2"]`) {
		t.Errorf("node affinity doesn't name n2: %s", b)
	}

	b, _ = json.Marshal(podManifest("x", Source{Namespace: "db", PVC: "data"}, ""))
	for _, unwanted := range []string{"affinity", "tolerations", "securityContext\":{\"run", "subPath", "readOnly"} {
		if strings.Contains(string(b), unwanted) {
			t.Errorf("plain manifest has %s: %s", unwanted, b)
		}
	}
	if !strings.Contains(string(b), DefaultImage) {
		t.Errorf("plain manifest doesn't use the default image: %s", b)
	}
}

func TestNames(t *testing.T) {
	if n := podName(strings.Repeat("a", 100) + ".b"); len(n) > 63 || !strings.HasPrefix(n, "tether-aaa") {
		t.Errorf("podName = %q", n)
	}
	if n := podName("data.pg-0"); !dnsLabel.MatchString(n) {
		t.Errorf("podName = %q isn't a DNS label", n)
	}
	if v := labelValue("Ryan@my host!" + strings.Repeat("x", 80)); len(v) > 63 || strings.ContainsAny(v, "@ !") {
		t.Errorf("labelValue = %q", v)
	}
}

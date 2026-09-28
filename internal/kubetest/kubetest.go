// Package kubetest installs a fake kubectl for tests: just enough of kubectl
// for tether's PVC mounts, with claims backed by local directories and
// "helper pods" that are files. `kubectl exec` runs the local sftp-server in
// the claim's directory.
package kubetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanmwright/tether/internal/sshtest"
)

// Context is the fake's only (and current) context.
const Context = "test-ctx"

// Node is where the fake's pods run.
const Node = "node-1"

type Fake struct {
	Dir string // state: pvcs/<ns>/<name>/ volumes, pods/<name> files, calls.log
}

// Install puts a fake kubectl first on PATH for the rest of the test. It
// skips the test if there's no local sftp-server to serve claims with.
func Install(t *testing.T) *Fake {
	t.Helper()
	sftp := sshtest.SFTPServer()
	if sftp == "" {
		t.Skip("sftp-server not found")
	}
	dir, err := os.MkdirTemp("", "tether-kube-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &Fake{Dir: dir}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, filepath.Join(dir, "pods"), filepath.Join(dir, "pvcs")} {
		os.MkdirAll(d, 0o755)
	}
	script := strings.NewReplacer("@STATE@", dir, "@SFTP@", sftp, "@CTX@", Context, "@NODE@", Node).Replace(fakeKubectl)
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// AddPVC creates a bound claim and returns its volume's directory.
func (f *Fake) AddPVC(t *testing.T, ns, name string) string {
	t.Helper()
	d := filepath.Join(f.Dir, "pvcs", ns, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// Pods lists the helper pods that exist.
func (f *Fake) Pods() []string {
	entries, _ := os.ReadDir(filepath.Join(f.Dir, "pods"))
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".claim") {
			out = append(out, e.Name())
		}
	}
	return out
}

// KillPod makes a pod go away, as if deleted by someone else.
func (f *Fake) KillPod(name string) {
	os.Remove(filepath.Join(f.Dir, "pods", name))
}

// Calls is every kubectl command line run so far.
func (f *Fake) Calls() string {
	b, _ := os.ReadFile(filepath.Join(f.Dir, "calls.log"))
	return string(b)
}

func (f *Fake) String() string { return fmt.Sprintf("fake kubectl in %s", f.Dir) }

const fakeKubectl = `#!/bin/sh
state=@STATE@
echo "$*" >> "$state/calls.log"
ctx=
while [ $# -gt 0 ]; do
	case $1 in
	--context) ctx=$2; shift 2 ;;
	--kubeconfig) shift 2 ;;
	*) break ;;
	esac
done
if [ -n "$ctx" ] && [ "$ctx" != @CTX@ ]; then
	echo "error: context \"$ctx\" does not exist" >&2; exit 1
fi
# the namespace, and the positional args after the subcommand
ns=default
cmd=$1; shift
rest=
while [ $# -gt 0 ]; do
	case $1 in
	-n) ns=$2; shift 2 ;;
	-c|-o|-l|-f|--field-selector) shift 2 ;;
	--) shift; break ;;
	-*) shift ;;
	*) rest="$rest $1"; shift ;;
	esac
done
set -- $rest "$@"

pvc_json() { # ns name
	printf '{"metadata":{"namespace":"%s","name":"%s"},"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"standard","resources":{"requests":{"storage":"1Gi"}}},"status":{"phase":"Bound","capacity":{"storage":"1Gi"}}}' "$1" "$2"
}

case $cmd in
version) echo "Client Version: v1.99.0-fake" ;;
auth) echo yes ;;
config)
	case $1 in
	current-context|get-contexts) echo @CTX@ ;;
	view) echo default ;;
	esac ;;
get)
	kind=$1; name=$2
	case $kind in
	pvc)
		if [ -n "$name" ]; then
			[ -d "$state/pvcs/$ns/$name" ] || { echo "Error from server (NotFound): persistentvolumeclaims \"$name\" not found" >&2; exit 1; }
			pvc_json "$ns" "$name"; exit 0
		fi
		printf '{"items":['; sep=
		for d in "$state"/pvcs/*/*; do
			[ -d "$d" ] || continue
			printf '%s' "$sep"; pvc_json "$(basename "$(dirname "$d")")" "$(basename "$d")"; sep=,
		done
		printf ']}\n' ;;
	pod)
		[ -e "$state/pods/$name" ] || { echo "Error from server (NotFound): pods \"$name\" not found" >&2; exit 1; }
		printf '{"spec":{"nodeName":"@NODE@"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}\n' ;;
	*) echo '{"items":[]}' ;;
	esac ;;
create)
	manifest=$(cat)
	name=$(printf '%s' "$manifest" | grep -o '"name":"tether-[^"]*"' | head -n1 | cut -d'"' -f4)
	claim=$(printf '%s' "$manifest" | grep -o '"claimName":"[^"]*"' | cut -d'"' -f4)
	ns=$(printf '%s' "$manifest" | grep -o '"namespace":"[^"]*"' | head -n1 | cut -d'"' -f4)
	[ -d "$state/pvcs/$ns/$claim" ] || { echo "claim $ns/$claim not found" >&2; exit 1; }
	echo "$ns/$claim" > "$state/pods/$name.claim"
	touch "$state/pods/$name"
	echo "pod/$name created" ;;
attach)
	pod="$state/pods/$1"
	[ -e "$pod" ] || { echo "error: pod $1 not found" >&2; exit 1; }
	# Background jobs get /dev/null as stdin unless it's redirected.
	exec 3<&0
	cat <&3 >/dev/null &
	c=$!
	while kill -0 $c 2>/dev/null && [ -e "$pod" ]; do sleep 0.1; done
	kill $c 2>/dev/null
	rm -f "$pod"
	[ -e "$pod.claim" ] && exit 0 ;;
exec)
	pod="$state/pods/$1"
	[ -e "$pod" ] || { echo "error: pod $1 not found" >&2; exit 1; }
	exec @SFTP@ -d "$state/pvcs/$(cat "$pod.claim")" ;;
delete)
	if [ "$1" = pods ] || [ "$1" = pod ] && [ -z "$2" ]; then exit 0; fi
	rm -f "$state/pods/$2" "$state/pods/$2.claim"
	echo "pod/$2 deleted" ;;
*) echo "fake kubectl: unsupported: $cmd" >&2; exit 1 ;;
esac
`

package mount

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ryanmwright/tether/internal/kube"
	"github.com/ryanmwright/tether/internal/linelog"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/sftpjail"
)

const (
	readyTimeout = 20 * time.Second
	stopTimeout  = 5 * time.Second
)

// Running is an active mount.
type Running struct {
	// Pod and Node are the helper pod behind a PVC mount and its node.
	Pod, Node string

	done chan struct{}
	once sync.Once
	err  error
	stop func(context.Context) error
}

func newRunning() *Running { return &Running{done: make(chan struct{})} }

func (r *Running) finish(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.done)
	})
}

// Done is closed when the mount goes away, whether stopped or failed.
func (r *Running) Done() <-chan struct{} { return r.done }

// Err says why the mount went away. Only valid after Done is closed.
func (r *Running) Err() error { return r.err }

// Stop unmounts, falling back to a lazy unmount if the mount is busy, and
// waits for the processes behind it to exit.
func (r *Running) Stop(ctx context.Context) error { return r.stop(ctx) }

// Start mounts spec over the connection m and returns once the mount is in
// place.
func Start(ctx context.Context, m *openssh.Master, spec Spec, log *slog.Logger) (*Running, error) {
	switch {
	case spec.Direction == PVCToLocal:
		return startPVC(ctx, m, spec, log)
	case m.IsLocal():
		return nil, errors.New("only PVC mounts are supported on the local host")
	case spec.Direction == LocalToRemote:
		return startLocalToRemote(ctx, m, spec, log)
	}
	return startRemoteToLocal(ctx, m, spec, log)
}

// startRemoteToLocal runs sshfs here, piped to the remote's SFTP server
// through an `ssh -s sftp` channel on the existing connection.
func startRemoteToLocal(ctx context.Context, m *openssh.Master, s Spec, log *slog.Logger) (*Running, error) {
	return startSSHFS(ctx, m.Command([]string{"-s"}, "sftp"), "ssh", sftpPath(s.Remote), s, log, nil)
}

// startPVC starts a helper pod holding the claim and runs sshfs here, piped
// to an sftp-server in the pod through `kubectl exec` on the connection.
func startPVC(ctx context.Context, m *openssh.Master, s Spec, log *slog.Logger) (*Running, error) {
	if _, err := exec.LookPath("sshfs"); err != nil {
		return nil, errSSHFSMissing
	}
	pod, err := kube.StartPod(ctx, m, *s.Kube, log)
	if err != nil {
		return nil, err
	}
	s.Options = withPVCCaching(s.Options)
	r, err := startSSHFS(ctx, pod.SFTPCommand(), "kubectl exec", ".", s, log, pod)
	if err != nil {
		pod.Stop(context.Background())
		return nil, err
	}
	r.Pod, r.Node = pod.Name, pod.Node
	return r, nil
}

var errSSHFSMissing = errors.New("sshfs not found locally; install it (Fedora: fuse-sshfs, Debian: sshfs)")

// startSSHFS runs sshfs here on the SFTP server that server runs (reading
// SFTP requests on its stdin), mounting path from it at s.Local. If pod is
// set, it goes with the mount: the mount ends when the pod does, and the pod
// is stopped after unmounting.
func startSSHFS(ctx context.Context, server *exec.Cmd, what, path string, s Spec, log *slog.Logger, pod *kube.Pod) (*Running, error) {
	sshfs, err := exec.LookPath("sshfs")
	if err != nil {
		return nil, errSSHFSMissing
	}
	if err := os.MkdirAll(s.Local, 0o755); err != nil {
		unmountLocal(s.Local, true) // maybe a stale mount in the way
		if err := os.MkdirAll(s.Local, 0o755); err != nil {
			return nil, err
		}
	}
	mp, err := filepath.EvalSymlinks(s.Local)
	if err != nil {
		return nil, err
	}
	if mounted(mp) {
		unmountLocal(mp, true) // left over from a lost connection
	}

	fs := exec.Command(sshfs, "-f", "-o", sshfsOptions(s.Options), ":"+path, mp)
	fs.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	srvErr, fsErr := linelog.New(log, what+" output"), linelog.New(log, "sshfs output")
	server.Stderr, fs.Stderr = srvErr, fsErr

	// Connect the two processes directly: the server's stdout is sshfs's
	// stdin and the other way around.
	toFS, fromSrv, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	toSrv, fromFS, err := os.Pipe()
	if err != nil {
		toFS.Close()
		fromSrv.Close()
		return nil, err
	}
	server.Stdin, server.Stdout = toSrv, fromSrv
	fs.Stdin, fs.Stdout = toFS, fromFS
	closePipes := func() {
		for _, f := range []*os.File{toFS, fromSrv, toSrv, fromFS} {
			f.Close()
		}
	}
	if err := server.Start(); err != nil {
		closePipes()
		return nil, err
	}
	if err := fs.Start(); err != nil {
		closePipes()
		server.Process.Kill()
		server.Wait()
		return nil, err
	}
	closePipes() // the children have their own copies

	r := newRunning()
	var podGone error
	var podMu sync.Mutex
	if pod != nil {
		go func() {
			select {
			case <-pod.Done():
				// The pod is gone, so the mount is dead: unmount so sshfs exits.
				podMu.Lock()
				podGone = pod.Err()
				podMu.Unlock()
				unmountLocal(mp, true)
				fs.Process.Signal(syscall.SIGTERM)
			case <-r.done:
			}
		}()
	}
	go func() {
		// When either side exits the other sees EOF and follows.
		fsWait := fs.Wait()
		server.Process.Signal(syscall.SIGTERM)
		server.Wait()
		if mounted(mp) {
			unmountLocal(mp, true)
		}
		podMu.Lock()
		gone := podGone
		podMu.Unlock()
		if gone != nil {
			r.finish(gone)
			return
		}
		r.finish(exitReason("sshfs", fsWait, fsErr.Last(), srvErr.Last()))
	}()
	r.stop = func(ctx context.Context) error {
		err := unmountLocal(mp, false)
		if err != nil {
			err = unmountLocal(mp, true)
		}
		err = waitOrKill(ctx, r, fs.Process, server.Process, err)
		if pod != nil {
			err = errors.Join(err, pod.Stop(ctx))
		}
		return err
	}

	if err := waitReady(ctx, r, func(context.Context) bool { return mounted(mp) }); err != nil {
		r.stop(context.Background())
		return nil, err
	}
	return r, nil
}

// remoteScript prefixes each remote shell snippet: it turns the path in $1
// into an absolute path $p, expanding ~ against the remote home.
const remoteScript = `case $1 in "~") p=$HOME ;; "~/"*) p=$HOME/${1#"~/"} ;; /*) p=$1 ;; *) p=$HOME/$1 ;; esac
unmount() { fusermount3 -u $1 "$p" 2>/dev/null || fusermount -u $1 "$p" 2>/dev/null; }
`

// mountScript clears any stale mount at $p, then runs sshfs on SFTP over
// stdin/stdout with the options in $2.
const mountScript = remoteScript + `command -v sshfs >/dev/null 2>&1 || { echo "sshfs not found on the remote; install it there" >&2; exit 3; }
unmount -z
mkdir -p "$p" || exit 3
exec sshfs -f -o "$2" :/ "$p"
`

const checkScript = remoteScript + `mountpoint -q "$p" 2>/dev/null || awk -v p="$p" '$2 == p { f = 1 } END { exit !f }' /proc/self/mounts`

const unmountScript = remoteScript + `unmount || unmount -z`

// remoteCommand is a command line that runs script under sh with args.
func remoteCommand(script string, args ...string) string {
	cmd := "sh -c " + shellQuote(script) + " tether-mount"
	for _, a := range args {
		cmd += " " + shellQuote(a)
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// startLocalToRemote runs sshfs on the remote and serves it SFTP from this
// side, confined to the shared directory.
func startLocalToRemote(ctx context.Context, m *openssh.Master, s Spec, log *slog.Logger) (*Running, error) {
	if fi, err := os.Stat(s.Local); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("local directory %s doesn't exist", s.Local)
	}
	ssh := m.Command(nil, remoteCommand(mountScript, s.Remote, sshfsOptions(s.Options)))
	sshErr := linelog.New(log, "remote sshfs output")
	ssh.Stderr = sshErr
	stdin, err := ssh.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := ssh.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := ssh.Start(); err != nil {
		return nil, err
	}

	r := newRunning()
	served := make(chan error, 1)
	go func() { served <- sftpjail.Serve(s.Local, pipeRWC{stdout, stdin}) }()
	go func() {
		sshWait := ssh.Wait()
		if err := <-served; err != nil {
			log.Warn("sftp server stopped", "err", err)
		}
		r.finish(exitReason("remote sshfs", sshWait, sshErr.Last(), ""))
	}()
	remoteRun := func(ctx context.Context, script string) error {
		ctx, cancel := context.WithTimeout(ctx, stopTimeout)
		defer cancel()
		_, err := m.Run(ctx, "", remoteCommand(script, s.Remote))
		return err
	}
	r.stop = func(ctx context.Context) error {
		return waitOrKill(ctx, r, ssh.Process, nil, remoteRun(ctx, unmountScript))
	}

	if err := waitReady(ctx, r, func(ctx context.Context) bool { return remoteRun(ctx, checkScript) == nil }); err != nil {
		r.stop(context.Background())
		return nil, err
	}
	return r, nil
}

type pipeRWC struct {
	io.ReadCloser
	io.WriteCloser
}

func (p pipeRWC) Close() error {
	return errors.Join(p.WriteCloser.Close(), p.ReadCloser.Close())
}

// waitReady polls isUp until the mount appears, fails, or times out.
func waitReady(ctx context.Context, r *Running, isUp func(context.Context) bool) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.done:
			return r.err
		case <-ctx.Done():
			return fmt.Errorf("mount not ready: %w", ctx.Err())
		case <-tick.C:
			if isUp(ctx) {
				return nil
			}
		}
	}
}

// waitOrKill waits for the mount's processes to exit after an unmount,
// killing them if they don't.
func waitOrKill(ctx context.Context, r *Running, a, b *os.Process, unmountErr error) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	select {
	case <-r.done:
		return unmountErr
	case <-ctx.Done():
	}
	for _, p := range []*os.Process{a, b} {
		if p != nil {
			p.Kill()
		}
	}
	<-r.done
	return errors.Join(unmountErr, errors.New("mount processes didn't exit; killed them"))
}

func exitReason(what string, err error, lines ...string) error {
	for _, l := range lines {
		if l != "" {
			return errors.New(l)
		}
	}
	if err != nil {
		return fmt.Errorf("%s exited: %w", what, err)
	}
	return fmt.Errorf("%s exited", what)
}

// unmountLocal unmounts a FUSE mount here, lazily if asked.
func unmountLocal(mp string, lazy bool) error {
	flag := "-u"
	if lazy {
		flag = "-uz"
	}
	var errs []error
	for _, bin := range fusermountCandidates() {
		out, err := exec.Command(bin, flag, mp).CombinedOutput()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %s", bin, strings.TrimSpace(string(out))))
	}
	return errors.Join(errs...)
}

// fusermountCandidates lists fusermount binaries to try: the NixOS setuid
// wrappers, then whatever is on PATH.
func fusermountCandidates() []string {
	var bins []string
	for _, name := range []string{"/run/wrappers/bin/fusermount3", "fusermount3", "/run/wrappers/bin/fusermount", "fusermount"} {
		if p, err := exec.LookPath(name); err == nil {
			bins = append(bins, p)
		}
	}
	return bins
}

// mounted reports whether mp is a mount point, from /proc/self/mountinfo.
// Unlike stat, this works for a FUSE mount whose server has died.
func mounted(mp string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescapeMountinfo(fields[4]) == mp {
			return true
		}
	}
	return false
}

// unescapeMountinfo decodes the octal escapes (\040 for space, etc.) that
// mountinfo uses in paths.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

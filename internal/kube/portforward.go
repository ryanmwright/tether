package kube

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/linelog"
	"github.com/ryanmwright/tether/internal/openssh"
)

const portForwardTimeout = 30 * time.Second

// PortForward is a running `kubectl port-forward` on a host.
type PortForward struct {
	// Port is where kubectl listens on the host.
	Port int

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{}
	err     error
	started time.Time
}

// Done is closed when kubectl exits: stopped, or its pod went away.
func (p *PortForward) Done() <-chan struct{} { return p.done }

// Err says why kubectl exited. Only valid after Done is closed.
func (p *PortForward) Err() error { return p.err }

// Uptime is how long kubectl has been running.
func (p *PortForward) Uptime() time.Duration { return time.Since(p.started) }

// Stop ends kubectl and waits for it.
func (p *PortForward) Stop() {
	p.stdin.Close() // the wrapper script kills kubectl
	select {
	case <-p.done:
	case <-time.After(stopTimeout):
		p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.done
	}
}

// portForwardScript runs the kubectl command in $1 and kills it when stdin
// closes: without a terminal, closing the ssh channel wouldn't stop it, and
// it would keep its port on the host.
//
// The watcher's output goes to /dev/null so it doesn't hold the session
// open after kubectl exits by itself.
//
// kubectl is exec'd in the background subshell, so $! is kubectl itself: a
// plain "eval ... &" would leave the subshell in between, and killing it
// would orphan kubectl.
const portForwardScript = `exec 3<&0
eval "exec $1" </dev/null &
k=$!
{ cat <&3; kill $k; } >/dev/null 2>&1 &
w=$!
wait $k
s=$?
kill $w 2>/dev/null
exit $s`

var forwardingFrom = regexp.MustCompile(`Forwarding from (?:127\.0\.0\.1|\[::1\]|[^ ]+):(\d+) ->`)

// StartPortForward runs `kubectl port-forward` on m to ref's port target,
// listening on bind:port there (port 0: any free one), and returns once it
// is listening.
func StartPortForward(ctx context.Context, m *openssh.Master, opts Options, ref forward.KubeRef, bind string, port, target int, log *slog.Logger) (*PortForward, error) {
	ports := fmt.Sprintf("%d:%d", port, target)
	if port == 0 {
		ports = fmt.Sprintf(":%d", target)
	}
	kc := kubectl(opts, ref.Context, "port-forward", "--address", bind, "-n", ref.Namespace, ref.Object(), ports)
	cmd := m.Command(nil, "sh -c "+Quote(portForwardScript)+" tether-port-forward "+Quote(kc))
	stderr := linelog.New(log, "kubectl port-forward output")
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &PortForward{cmd: cmd, stdin: stdin, done: make(chan struct{}), started: time.Now()}

	listening := make(chan int, 1)
	var once sync.Once
	go func() {
		// "Forwarding from 127.0.0.1:43127 -> 80", then a line per connection.
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwardingFrom.FindStringSubmatch(sc.Text()); m != nil {
				n, _ := strconv.Atoi(m[1])
				once.Do(func() { listening <- n })
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		switch msg := stderr.Last(); {
		case msg != "":
			p.err = errors.New(strings.TrimPrefix(msg, "error: "))
		case err != nil:
			p.err = fmt.Errorf("kubectl port-forward exited: %w", err)
		default:
			p.err = errors.New("kubectl port-forward exited")
		}
		close(p.done)
	}()

	ctx, cancel := context.WithTimeout(ctx, portForwardTimeout)
	defer cancel()
	select {
	case p.Port = <-listening:
		return p, nil
	case <-p.done:
		return nil, p.err
	case <-ctx.Done():
		p.Stop()
		return nil, fmt.Errorf("kubectl port-forward didn't start listening: %w", ctx.Err())
	}
}

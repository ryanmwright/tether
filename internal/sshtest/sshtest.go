// Package sshtest runs a throwaway, unprivileged sshd on loopback for
// integration tests. The current user logs in to it with a generated key.
package sshtest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// HostAlias is the ssh_config alias that reaches the server.
const HostAlias = "testhost"

type Server struct {
	Dir        string
	Port       int
	ConfigFile string // ssh_config defining HostAlias
	sshd       string
	cmd        *exec.Cmd
}

// Start runs sshd for the duration of the test. It skips the test when sshd
// isn't available or can't accept logins in this environment.
func Start(t *testing.T) *Server {
	t.Helper()
	return StartWithConfig(t, "")
}

// StartWithConfig is Start with extra sshd_config lines, e.g. SetEnv to give
// remote sessions their own environment.
func StartWithConfig(t *testing.T, sshdExtra string) *Server {
	t.Helper()
	sshd := findSSHD()
	if sshd == "" {
		t.Skip("sshd not found (set TETHER_TEST_SSHD)")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not found")
	}
	if reason := loginUnavailable(); reason != "" {
		t.Skip(reason)
	}

	// Unix socket paths are limited to ~108 bytes, so avoid t.TempDir's
	// long names.
	dir, err := os.MkdirTemp("", "tether-sshd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &Server{Dir: dir, Port: freePort(t), ConfigFile: filepath.Join(dir, "ssh_config"), sshd: sshd}

	for _, key := range []string{"hostkey", "userkey"} {
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, key)).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen: %v: %s", err, out)
		}
	}
	pub, _ := os.ReadFile(filepath.Join(dir, "userkey.pub"))
	write(t, filepath.Join(dir, "authorized_keys"), string(pub))
	write(t, filepath.Join(dir, "sshd_config"), fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %[2]s/hostkey
AuthorizedKeysFile %[2]s/authorized_keys
PidFile none
StrictModes no
UsePAM no
StreamLocalBindUnlink yes
LogLevel ERROR
Subsystem sftp %[3]s
%[4]s
`, s.Port, dir, findSFTPServer(sshd), sshdExtra))
	u, _ := user.Current()
	write(t, s.ConfigFile, fmt.Sprintf(`Host %s
  HostName 127.0.0.1
  Port %d
  User %s
  IdentityFile %s/userkey
  IdentitiesOnly yes
  IdentityAgent none
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
`, HostAlias, s.Port, u.Username, dir))

	t.Cleanup(s.Stop)
	s.Restart(t)
	return s
}

// Restart starts sshd again on the same port after Stop.
func (s *Server) Restart(t *testing.T) {
	t.Helper()
	s.cmd = exec.Command(s.sshd, "-D", "-e", "-f", filepath.Join(s.Dir, "sshd_config"))
	logFile, _ := os.Create(filepath.Join(s.Dir, "sshd.log"))
	s.cmd.Stderr = logFile
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", s.Addr()); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	log, _ := os.ReadFile(logFile.Name())
	t.Fatalf("sshd did not start: %s", log)
}

func (s *Server) Addr() string { return fmt.Sprintf("127.0.0.1:%d", s.Port) }

// Stop kills sshd and every session it spawned, simulating the server going
// away.
func (s *Server) Stop() {
	if s.cmd == nil || s.cmd.Process == nil || s.cmd.ProcessState != nil {
		return
	}
	// Per-connection sshd-session processes outlive the listener, so kill
	// the whole tree.
	for _, pid := range descendants(s.cmd.Process.Pid) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	s.cmd.Process.Kill()
	s.cmd.Wait()
}

// descendants lists the children of pid, recursively, from /proc.
func descendants(pid int) []int {
	children := map[int][]int{}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, path := range stats {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Fields after the parenthesized command: state ppid ...
		rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
		var state string
		var child, ppid int
		fmt.Sscan(filepath.Base(filepath.Dir(path)), &child)
		fmt.Sscan(rest, &state, &ppid)
		children[ppid] = append(children[ppid], child)
	}
	var out []int
	var walk func(int)
	walk = func(p int) {
		for _, c := range children[p] {
			out = append(out, c)
			walk(c)
		}
	}
	walk(pid)
	return out
}

// SFTPServer is the path of OpenSSH's sftp-server, or "" if it can't be
// found.
func SFTPServer() string {
	sshd := findSSHD()
	if sshd == "" {
		return ""
	}
	if p := findSFTPServer(sshd); p != "internal-sftp" {
		return p
	}
	return ""
}

// findSFTPServer finds sftp-server next to sshd (Nix: ../libexec) or in the
// usual distribution locations.
func findSFTPServer(sshd string) string {
	candidates := []string{
		filepath.Join(filepath.Dir(sshd), "..", "libexec", "sftp-server"),
		"/usr/lib/openssh/sftp-server", "/usr/libexec/openssh/sftp-server", "/usr/lib/ssh/sftp-server",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return filepath.Clean(p)
		}
	}
	return "internal-sftp"
}

func findSSHD() string {
	if p := os.Getenv("TETHER_TEST_SSHD"); p != "" {
		return p
	}
	if p, err := exec.LookPath("sshd"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/sshd", "/usr/bin/sshd"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// loginUnavailable reports why sshd would refuse the current user, e.g. in
// the Nix build sandbox where the build user's shell doesn't exist.
func loginUnavailable() string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return "no /etc/passwd"
	}
	defer f.Close()
	uid := fmt.Sprint(syscall.Getuid())
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 7 && fields[2] == uid {
			if _, err := os.Stat(fields[6]); err != nil {
				return fmt.Sprintf("login shell %s doesn't exist; sshd would refuse login", fields[6])
			}
			return ""
		}
	}
	return "current user not in /etc/passwd"
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// FreePort returns a loopback TCP port that was free a moment ago.
func FreePort(t *testing.T) int { return freePort(t) }

// EchoServer listens on loopback and echoes back whatever it receives.
func EchoServer(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write(buf[:n])
				}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// Echo dials network/addr, sends msg and returns the reply.
func Echo(network, addr, msg string) (string, error) {
	c, err := net.DialTimeout(network, addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	buf := make([]byte, len(msg))
	n, err := c.Read(buf)
	return string(buf[:n]), err
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

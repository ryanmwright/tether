package openssh

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os/exec"
	"strings"
)

// Address is the host:port ssh connects to for dest, as ssh's config
// resolves it, or "" if it goes through a proxy (ProxyJump, ProxyCommand)
// and so can't be reached directly.
func Address(ctx context.Context, opts Options, dest string) (string, error) {
	args := append(opts.baseArgs(), "-G", "--", dest)
	out, err := exec.CommandContext(ctx, opts.binary(), args...).Output()
	if err != nil {
		return "", err
	}
	return parseAddress(out), nil
}

// parseAddress reads the address from `ssh -G` output.
func parseAddress(out []byte) string {
	var host, port string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, value, _ := strings.Cut(sc.Text(), " ")
		switch strings.ToLower(key) {
		case "hostname":
			host = value
		case "port":
			port = value
		case "proxyjump", "proxycommand":
			if value != "none" {
				return ""
			}
		}
	}
	if host == "" || port == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

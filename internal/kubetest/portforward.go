package kubetest

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// RunHelper serves the fake kubectl's port-forward when the test binary is
// run as it (from TestMain), and exits; otherwise it does nothing.
func RunHelper() {
	if os.Getenv("TETHER_KUBETEST_PORT_FORWARD") == "" {
		return
	}
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "error: usage: ADDRESS TARGET-FILE [LOCAL]:REMOTE")
		os.Exit(2)
	}
	os.Exit(portForward(os.Args[1], os.Args[2], os.Args[3]))
}

// portForward acts like `kubectl port-forward --address addr OBJ ports`: it
// listens, says where, relays connections to the address in targetFile, and
// exits when that file is removed (the pod went away).
func portForward(addr, targetFile, ports string) int {
	b, err := os.ReadFile(targetFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: services not found")
		return 1
	}
	target := strings.TrimSpace(string(b))
	local, remote, _ := strings.Cut(ports, ":")
	if local == "" {
		local = "0"
	}
	l, err := net.Listen("tcp", net.JoinHostPort(addr, local))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: unable to listen on any of the requested ports:", err)
		return 1
	}
	fmt.Printf("Forwarding from %s -> %s\n", l.Addr(), remote)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			fmt.Printf("Handling connection for %s\n", remote)
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return // kubectl closes the connection when the pod refuses
				}
				defer up.Close()
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	for {
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(targetFile); err != nil {
			fmt.Fprintln(os.Stderr, "error: lost connection to pod")
			return 1
		}
	}
}

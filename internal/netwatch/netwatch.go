// Package netwatch reports network changes (links, addresses and routes
// coming or going) using a netlink route socket.
package netwatch

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// Multicast groups from <linux/rtnetlink.h>.
const (
	rtmgrpLink       = 0x1
	rtmgrpIPv4Ifaddr = 0x10
	rtmgrpIPv4Route  = 0x40
	rtmgrpIPv6Ifaddr = 0x100
	rtmgrpIPv6Route  = 0x400

	groups = rtmgrpLink | rtmgrpIPv4Ifaddr | rtmgrpIPv4Route | rtmgrpIPv6Ifaddr | rtmgrpIPv6Route
)

// Watch returns a channel that receives a value once changes have settled
// for the debounce period. Bursts (e.g. a Wi-Fi reconnect) produce a single
// event. The channel is closed when ctx is done.
func Watch(ctx context.Context, debounce time.Duration) (<-chan struct{}, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: groups}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	// A non-blocking fd wrapped in os.File uses the runtime poller, so Close
	// unblocks a pending Read.
	f := os.NewFile(uintptr(fd), "netlink")
	context.AfterFunc(ctx, func() { f.Close() })

	raw := make(chan struct{}, 1)
	go func() {
		defer close(raw)
		buf := make([]byte, 64*1024)
		for {
			// ENOBUFS means events were dropped, which is still a change.
			if _, err := f.Read(buf); err != nil && !errors.Is(err, syscall.ENOBUFS) {
				return
			}
			select {
			case raw <- struct{}{}:
			default:
			}
		}
	}()

	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		var timer <-chan time.Time
		for {
			select {
			case _, ok := <-raw:
				if !ok {
					return
				}
				timer = time.After(debounce)
			case <-timer:
				timer = nil
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out, nil
}

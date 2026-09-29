package forward

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DialSOCKS connects to target ("host:port") through the SOCKS5 proxy at
// socksAddr, asking the proxy to resolve the host name.
func DialSOCKS(ctx context.Context, socksAddr, target string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 || len(host) > 255 {
		return nil, fmt.Errorf("invalid target %q", target)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", socksAddr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(30 * time.Second))
	}
	fail := func(err error) (net.Conn, error) {
		c.Close()
		return nil, fmt.Errorf("SOCKS proxy: %w", err)
	}
	// Greeting: version 5, one method, no authentication.
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return fail(err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		return fail(errors.New("no acceptable authentication method"))
	}
	// CONNECT to a domain name (or an IP address as text; the proxy copes).
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		req = append(append(req, 1), ip.To4()...)
	} else if ip != nil {
		req = append(append(req, 4), ip.To16()...)
	} else {
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return fail(err)
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return fail(err)
	}
	if head[1] != 0 {
		return fail(fmt.Errorf("connecting to %s: %s", target, socksReply(head[1])))
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return fail(err)
		}
		skip = int(n[0])
	default:
		return fail(errors.New("bad reply"))
	}
	if _, err := io.ReadFull(c, make([]byte, skip+2)); err != nil {
		return fail(err)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

func socksReply(code byte) string {
	switch code {
	case 1:
		return "general failure"
	case 2:
		return "not allowed"
	case 3:
		return "network unreachable"
	case 4:
		return "host unreachable"
	case 5:
		return "connection refused"
	case 6:
		return "timed out"
	}
	return fmt.Sprintf("error %d", code)
}

// Proxy is tether's HTTP proxy: it accepts HTTP proxy requests (CONNECT,
// and plain requests for http:// URLs) and SOCKS5, and makes every
// connection through the SOCKS proxy at SOCKS (ssh's dynamic forward).
type Proxy struct {
	SOCKS string
	Log   *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// Serve accepts connections on l until it's closed, then closes the
// connections still open.
func (p *Proxy) Serve(l net.Listener) {
	p.mu.Lock()
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		for c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *Proxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
}

func (p *Proxy) handle(c net.Conn) {
	p.track(c, true)
	defer p.track(c, false)
	defer c.Close()
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 5 {
		// SOCKS5: hand the whole conversation to ssh's proxy.
		up, err := net.Dial("tcp", p.SOCKS)
		if err != nil {
			return
		}
		p.track(up, true)
		defer p.track(up, false)
		pipe(c, br, up)
		return
	}

	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if req.Method == http.MethodConnect {
		up, err := DialSOCKS(ctx, p.SOCKS, req.Host)
		if err != nil {
			p.fail(c, err)
			return
		}
		p.track(up, true)
		defer p.track(up, false)
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		pipe(c, br, up)
		return
	}
	if req.URL.Scheme != "http" || req.URL.Host == "" {
		p.fail(c, fmt.Errorf("not a proxy request: %s %s", req.Method, req.RequestURI))
		return
	}
	target := req.URL.Host
	if req.URL.Port() == "" {
		target = net.JoinHostPort(req.URL.Hostname(), "80")
	}
	up, err := DialSOCKS(ctx, p.SOCKS, target)
	if err != nil {
		p.fail(c, err)
		return
	}
	p.track(up, true)
	defer p.track(up, false)
	// One request per connection, so a client reusing it for another host
	// doesn't end up at the first one.
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	req.Header.Set("Connection", "close")
	req.Close = true
	if err := req.Write(up); err != nil {
		return
	}
	pipe(c, br, up)
}

func (p *Proxy) fail(c net.Conn, err error) {
	if p.Log != nil {
		p.Log.Debug("proxy request failed", "err", err)
	}
	msg := err.Error() + "\n"
	fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
}

// pipe copies between the client (c, with what's buffered in br) and up.
// When up is done sending, both connections are closed: a client holding
// its connection open (keep-alive) would otherwise keep this going.
func pipe(c net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{})
	go func() {
		io.Copy(up, br)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		close(done)
	}()
	io.Copy(c, up)
	c.Close()
	up.Close()
	<-done
}

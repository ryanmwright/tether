package forward

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeSOCKS is a minimal SOCKS5 server (no auth, CONNECT) that connects
// directly, standing in for ssh's dynamic forward.
func fakeSOCKS(t *testing.T) string {
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
				buf := make([]byte, 262)
				io.ReadFull(c, buf[:2])
				io.ReadFull(c, buf[:buf[1]])
				c.Write([]byte{5, 0})
				io.ReadFull(c, buf[:4])
				var host string
				switch buf[3] {
				case 1:
					io.ReadFull(c, buf[:4])
					host = net.IP(buf[:4]).String()
				case 3:
					io.ReadFull(c, buf[:1])
					n := int(buf[0])
					io.ReadFull(c, buf[:n])
					host = string(buf[:n])
				}
				io.ReadFull(c, buf[:2])
				port := binary.BigEndian.Uint16(buf[:2])
				up, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	return l.Addr().String()
}

func startProxy(t *testing.T) (proxy, socks string) {
	t.Helper()
	socks = fakeSOCKS(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&Proxy{SOCKS: socks}).Serve(l)
	return l.Addr().String(), socks
}

func TestProxyHTTP(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	defer web.Close()
	proxy, _ := startProxy(t)

	// Plain http:// through the proxy, as HTTP_PROXY clients do.
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustURL(t, "http://"+proxy))}, Timeout: 5 * time.Second}
	resp, err := client.Get(web.URL + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello /plain" {
		t.Errorf("plain request: %q", body)
	}

	// CONNECT, as HTTPS clients do.
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	host := strings.TrimPrefix(web.URL, "http://")
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT reply: %q", line)
	}
	br.ReadString('\n') // blank line
	fmt.Fprintf(c, "GET /tunnel HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	if string(body) != "hello /tunnel" {
		t.Errorf("through CONNECT: %q", body)
	}

	// Unreachable targets get a 502 saying why.
	resp, err = client.Get("http://127.0.0.1:1/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "refused") {
		t.Errorf("unreachable: %d %q", resp.StatusCode, body)
	}
}

func TestProxySOCKSPassthrough(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "via socks") }))
	defer web.Close()
	proxy, _ := startProxy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialSOCKS(ctx, proxy, strings.TrimPrefix(web.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via socks" {
		t.Errorf("SOCKS through the HTTP proxy port: %q", body)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func startServer(t *testing.T, register func(*Server)) (socket string, cancel context.CancelFunc, served chan error) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(slog.New(slog.DiscardHandler))
	register(srv)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return socket, cancel, done
}

func dial(t *testing.T, socket string) *Client {
	t.Helper()
	c, err := Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCall(t *testing.T) {
	socket, _, _ := startServer(t, func(s *Server) {
		s.Handle("echo", func(_ context.Context, p json.RawMessage) (any, error) {
			var v map[string]int
			if err := json.Unmarshal(p, &v); err != nil {
				return nil, Errorf(CodeInvalidParams, "bad params: %v", err)
			}
			v["n"]++
			return v, nil
		})
		s.Handle("fail", func(context.Context, json.RawMessage) (any, error) {
			return nil, errors.New("boom")
		})
	})
	c := dial(t, socket)
	ctx := context.Background()

	var got map[string]int
	if err := c.Call(ctx, "echo", map[string]int{"n": 1}, &got); err != nil || got["n"] != 2 {
		t.Fatalf("echo: %v %v", got, err)
	}

	for method, code := range map[string]int{"fail": CodeInternalError, "missing": CodeMethodNotFound} {
		err := c.Call(ctx, method, nil, nil)
		if rpcErr, ok := errors.AsType[*Error](err); !ok || rpcErr.Code != code {
			t.Errorf("%s: got %v, want code %d", method, err, code)
		}
	}
	if err := c.Call(ctx, "echo", "not an object", nil); err == nil || !strings.Contains(err.Error(), "bad params") {
		t.Errorf("invalid params: %v", err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	release := make(chan struct{})
	socket, _, _ := startServer(t, func(s *Server) {
		s.Handle("slow", func(context.Context, json.RawMessage) (any, error) {
			<-release
			return "slow", nil
		})
		s.Handle("fast", func(context.Context, json.RawMessage) (any, error) { return "fast", nil })
	})
	c := dial(t, socket)
	ctx := context.Background()

	var wg sync.WaitGroup
	var slow string
	wg.Go(func() { c.Call(ctx, "slow", nil, &slow) })

	// A slow call must not block others on the same connection.
	var fast string
	if err := c.Call(ctx, "fast", nil, &fast); err != nil || fast != "fast" {
		t.Fatalf("fast: %q %v", fast, err)
	}
	close(release)
	wg.Wait()
	if slow != "slow" {
		t.Errorf("slow = %q", slow)
	}
}

func TestShutdownFinishesInflightCall(t *testing.T) {
	var cancel context.CancelFunc
	socket, cancel, served := startServer(t, func(s *Server) {
		s.Handle("stop", func(context.Context, json.RawMessage) (any, error) {
			cancel()
			return "bye", nil
		})
	})
	c := dial(t, socket)

	var got string
	if err := c.Call(context.Background(), "stop", nil, &got); err != nil || got != "bye" {
		t.Fatalf("stop: %q %v", got, err)
	}
	select {
	case err := <-served:
		served <- err // for cleanup
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	if err := c.Call(context.Background(), "stop", nil, nil); err == nil {
		t.Errorf("call after shutdown: %v", err)
	}
}

func TestMalformedInput(t *testing.T) {
	socket, _, _ := startServer(t, func(s *Server) {})
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	conn.Write([]byte(`{"jsonrpc":"1.0","id":7,"method":"x"}` + "\n"))
	line, _ := r.ReadString('\n')
	if !strings.Contains(line, `"id":7`) || !strings.Contains(line, `-32600`) {
		t.Errorf("invalid request response: %s", line)
	}

	conn.Write([]byte("{not json\n"))
	line, _ = r.ReadString('\n')
	if !strings.Contains(line, `-32700`) {
		t.Errorf("parse error response: %s", line)
	}
}

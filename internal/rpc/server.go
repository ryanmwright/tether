package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// HandlerFunc handles one method call. The returned value is marshalled as
// the result.
type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

type Server struct {
	log      *slog.Logger
	mu       sync.RWMutex
	handlers map[string]HandlerFunc
}

func NewServer(log *slog.Logger) *Server {
	return &Server{log: log, handlers: map[string]HandlerFunc{}}
}

func (s *Server) Handle(method string, h HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Serve accepts connections until ctx is cancelled. On shutdown it stops
// reading new requests, lets in-flight calls write their responses, then
// closes every connection before returning.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()

	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		conns.Go(func() { s.serveConn(ctx, c) })
	}
}

func (s *Server) serveConn(ctx context.Context, c net.Conn) {
	w := &writer{c: c}
	// Handlers get a context that lives as long as the connection, so they
	// can keep pushing notifications after returning.
	ctx, cancel := context.WithCancel(context.WithValue(ctx, connKey{}, &Conn{w: w}))
	var inflight sync.WaitGroup
	defer func() {
		cancel()
		inflight.Wait()
		c.Close()
	}()
	// Unblock the pending read on shutdown; writes keep working so in-flight
	// calls can still respond.
	stop := context.AfterFunc(ctx, func() { c.SetReadDeadline(time.Now()) })
	defer stop()

	dec := json.NewDecoder(bufio.NewReader(c))
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			var syntaxErr *json.SyntaxError
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
				// The stream can't be resynchronized after malformed JSON.
				w.write(&message{ID: json.RawMessage("null"), Error: Errorf(CodeParseError, "parse error: %v", err)})
			}
			return
		}
		if m.JSONRPC != Version || m.Method == "" {
			if len(m.ID) > 0 {
				w.write(&message{ID: m.ID, Error: Errorf(CodeInvalidRequest, "invalid request")})
			}
			continue
		}
		inflight.Go(func() { s.dispatch(ctx, w, &m) })
	}
}

// Conn lets a handler send notifications to its client.
type Conn struct{ w *writer }

type connKey struct{}

// ConnFromContext returns the connection a handler was called on.
func ConnFromContext(ctx context.Context) *Conn {
	c, _ := ctx.Value(connKey{}).(*Conn)
	return c
}

// Notify sends a notification (a request without an id) to the client.
func (c *Conn) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.w.write(&message{Method: method, Params: b})
}

func (s *Server) dispatch(ctx context.Context, w *writer, req *message) {
	s.mu.RLock()
	h, ok := s.handlers[req.Method]
	s.mu.RUnlock()

	resp := &message{ID: req.ID}
	if !ok {
		resp.Error = Errorf(CodeMethodNotFound, "method not found: %s", req.Method)
	} else if result, err := h(ctx, req.Params); err != nil {
		if rpcErr, ok := errors.AsType[*Error](err); ok {
			resp.Error = rpcErr
		} else {
			resp.Error = &Error{Code: CodeInternalError, Message: err.Error()}
		}
	} else if resp.Result, err = json.Marshal(result); err != nil {
		resp.Result = nil
		resp.Error = Errorf(CodeInternalError, "encoding result: %v", err)
	}

	if len(req.ID) == 0 {
		return // notification: no response
	}
	if err := w.write(resp); err != nil {
		s.log.Debug("rpc: writing response", "method", req.Method, "err", err)
	}
}

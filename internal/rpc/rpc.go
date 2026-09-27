// Package rpc implements a minimal JSON-RPC 2.0 transport over stream
// connections. Each message is a single JSON value terminated by a newline.
package rpc

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
)

const Version = "2.0"

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error is a JSON-RPC error object. Handlers may return one to control the
// code sent to the client; any other error is reported as CodeInternalError.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// message is the union of requests, responses and notifications, so either
// side can decode whatever arrives next.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// writer serializes whole messages onto a connection.
type writer struct {
	mu sync.Mutex
	c  net.Conn
}

func (w *writer) write(m *message) error {
	m.JSONRPC = Version
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.c.Write(b)
	return err
}

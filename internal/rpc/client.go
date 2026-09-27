package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
)

var ErrClosed = errors.New("rpc: connection closed")

// Client issues calls over a single connection. It is safe for concurrent use.
type Client struct {
	w      *writer
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan *message
	err     error // set once the read loop exits
	done    chan struct{}
}

func Dial(ctx context.Context, socketPath string) (*Client, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	return NewClient(c), nil
}

func NewClient(c net.Conn) *Client {
	cl := &Client{
		w:       &writer{c: c},
		pending: map[int64]chan *message{},
		done:    make(chan struct{}),
	}
	go cl.readLoop()
	return cl
}

func (c *Client) Close() error {
	err := c.w.c.Close()
	<-c.done
	return err
}

// Call invokes method and decodes the result into result, which may be nil.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	req := &message{Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	id := c.nextID.Add(1)
	req.ID = json.RawMessage(strconv.FormatInt(id, 10))

	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.w.write(req); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, result)
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) readLoop() {
	dec := json.NewDecoder(bufio.NewReader(c.w.c))
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			break
		}
		id, perr := strconv.ParseInt(string(m.ID), 10, 64)
		if perr != nil {
			continue // notification or unknown id
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			ch <- &m
		}
	}
	c.mu.Lock()
	c.err = ErrClosed
	c.mu.Unlock()
	close(c.done)
}

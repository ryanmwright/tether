package usbip

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client talks to the helper for the daemon. It keeps one connection open:
// the helper takes back what was exported when it closes.
type Client struct {
	Socket string

	mu   sync.Mutex
	conn net.Conn
	sc   *bufio.Scanner
	port int
}

// ErrNoHelper means the helper isn't running.
var ErrNoHelper = errors.New("the USB/IP helper isn't running; set it up with `tether usbip-helper install`")

// Port is the helper's USB/IP port on 127.0.0.1.
func (c *Client) Port(ctx context.Context) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dial(ctx); err != nil {
		return 0, err
	}
	return c.port, nil
}

// Export binds busid for sharing. It stays exported while the client's
// connection lasts; Connected tells whether that is still the case.
func (c *Client) Export(ctx context.Context, busid string) error {
	_, err := c.call(ctx, request{Op: "export", BusID: busid})
	return err
}

// Unexport gives busid back to this machine.
func (c *Client) Unexport(ctx context.Context, busid string) error {
	_, err := c.call(ctx, request{Op: "unexport", BusID: busid})
	return err
}

// Close drops the connection, which unexports everything.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
}

func (c *Client) call(ctx context.Context, req request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dial(ctx); err != nil {
		return response{}, err
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		c.drop()
		return resp, fmt.Errorf("USB/IP helper: %w", err)
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func (c *Client) dial(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return ErrNoHelper
	}
	c.conn, c.sc = conn, bufio.NewScanner(conn)
	resp, err := c.roundTrip(ctx, request{Op: "hello"})
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	if err != nil {
		c.drop()
		return fmt.Errorf("USB/IP helper: %w", err)
	}
	c.port = resp.Port
	return nil
}

func (c *Client) roundTrip(ctx context.Context, req request) (response, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(setupTimeout)
	}
	c.conn.SetDeadline(deadline)
	defer c.conn.SetDeadline(time.Time{})
	if err := json.NewEncoder(c.conn).Encode(req); err != nil {
		return response{}, err
	}
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return response{}, err
		}
		return response{}, errors.New("connection closed")
	}
	var resp response
	err := json.Unmarshal(c.sc.Bytes(), &resp)
	return resp, err
}

func (c *Client) drop() {
	if c.conn != nil {
		c.conn.Close()
		c.conn, c.sc, c.port = nil, nil, 0
	}
}

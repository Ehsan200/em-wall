package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type Client struct {
	path string

	mu     sync.Mutex
	conn   net.Conn
	r      *bufio.Reader
	nextID int64
}

func Dial(socketPath string) (*Client, error) {
	c := &Client{path: socketPath}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

func (c *Client) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.Dial("unix", c.path)
	if err != nil {
		return fmt.Errorf("ipc: dial %s: %w", c.path, err)
	}
	c.conn = conn
	c.r = bufio.NewReader(conn)
	return nil
}

// Call makes one synchronous request. result may be nil to discard.
func (c *Client) Call(method string, params, result any) error {
	if err := c.connect(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("ipc: marshal params: %w", err)
		}
		paramsRaw = b
	}
	req := Request{ID: id, Method: method, Params: paramsRaw}
	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ipc: marshal req: %w", err)
	}
	line = append(line, '\n')
	if _, err := c.conn.Write(line); err != nil {
		c.dropLocked()
		return fmt.Errorf("ipc: write: %w", err)
	}

	respBytes, err := c.r.ReadBytes('\n')
	if err != nil {
		c.dropLocked()
		return fmt.Errorf("ipc: read: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return fmt.Errorf("ipc: unmarshal resp: %w", err)
	}
	if resp.Error != nil {
		return errors.New(resp.Error.Message)
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("ipc: unmarshal result: %w", err)
		}
	}
	return nil
}

func (c *Client) dropLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.r = nil
	}
}

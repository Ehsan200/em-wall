package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestServerClient_RoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sock, nil)
	srv.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var p map[string]string
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return map[string]string{"reply": p["msg"]}, nil
	})
	srv.Handle("boom", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, errors.New("kaboom")
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(srv.Shutdown)

	// Wait briefly for the socket to appear.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c, err := Dial(sock); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	c, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var got map[string]string
	if err := c.Call("echo", map[string]string{"msg": "hi"}, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got["reply"] != "hi" {
		t.Errorf("reply = %q, want hi", got["reply"])
	}

	if err := c.Call("boom", nil, nil); err == nil || err.Error() != "kaboom" {
		t.Errorf("expected kaboom error, got %v", err)
	}

	if err := c.Call("missing.method", nil, nil); err == nil {
		t.Errorf("expected unknown_method error")
	}
}

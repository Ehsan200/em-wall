package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
)

// Handler returns a result (any json-marshalable value) or an error
// for a given method invocation.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type Server struct {
	socketPath string
	handlers   map[string]Handler
	logger     *log.Logger

	mu       sync.Mutex
	listener net.Listener
}

func NewServer(socketPath string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		socketPath: socketPath,
		handlers:   make(map[string]Handler),
		logger:     logger,
	}
}

func (s *Server) Handle(method string, h Handler) { s.handlers[method] = h }

func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return fmt.Errorf("ipc: mkdir socket dir: %w", err)
	}
	_ = os.Remove(s.socketPath)
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("ipc: listen: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o660); err != nil {
		_ = l.Close()
		return fmt.Errorf("ipc: chmod: %w", err)
	}
	// When running as root, hand group ownership to "staff" so the
	// regular user (a staff member by default on macOS) can connect.
	if os.Geteuid() == 0 {
		if g, err := user.LookupGroup("staff"); err == nil {
			if gid, err := strconv.Atoi(g.Gid); err == nil {
				if err := os.Chown(s.socketPath, -1, gid); err != nil {
					s.logger.Printf("ipc: chown staff: %v (continuing)", err)
				}
			}
		}
	}
	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.logger.Printf("ipc: accept: %v", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.socketPath)
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.logger.Printf("ipc: read: %v", err)
			}
			return
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(Response{Error: &ErrorBody{Code: "bad_request", Message: err.Error()}})
			continue
		}
		resp := s.dispatch(ctx, req)
		if err := enc.Encode(resp); err != nil {
			s.logger.Printf("ipc: write: %v", err)
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	h, ok := s.handlers[req.Method]
	if !ok {
		// Be explicit so the UI doesn't render a bare method name as an
		// error toast. The most common cause is a fresh app talking to
		// an older daemon binary that never registered this handler —
		// in that case re-install em-wall from Settings to pick up the
		// new daemon.
		return Response{ID: req.ID, Error: &ErrorBody{
			Code:    "unknown_method",
			Message: "daemon doesn't recognise method " + req.Method + " — your installed daemon is older than this app, reinstall via Settings → Uninstall → Install",
		}}
	}
	result, err := h(ctx, req.Params)
	if err != nil {
		return Response{ID: req.ID, Error: &ErrorBody{Code: "handler_error", Message: err.Error()}}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return Response{ID: req.ID, Error: &ErrorBody{Code: "marshal_error", Message: err.Error()}}
	}
	return Response{ID: req.ID, Result: raw}
}

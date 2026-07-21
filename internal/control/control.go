package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/supervisor"
)

type Request struct {
	Action string   `json:"action"`
	Names  []string `json:"names,omitempty"`
}

type Response struct {
	OK        bool                `json:"ok"`
	Message   string              `json:"message,omitempty"`
	Processes []supervisor.Status `json:"processes,omitempty"`
}

func Call(socket string, request Request) (Response, error) {
	connection, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return Response{}, fmt.Errorf("connect to %s: %w", socket, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return Response{}, fmt.Errorf("send request: %w", err)
	}
	var response Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	return response, nil
}

type Server struct {
	socket     string
	configPath string
	runtime    config.Config
	cancel     context.CancelFunc
	logger     *log.Logger

	mu      sync.RWMutex
	manager *supervisor.Manager
}

func NewServer(cfg config.Config, configPath string, manager *supervisor.Manager, cancel context.CancelFunc, logger *log.Logger) *Server {
	return &Server{socket: cfg.Socket, configPath: configPath, runtime: cfg, manager: manager, cancel: cancel, logger: logger}
}

func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o755); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := prepareSocket(s.socket); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.socket, err)
	}
	defer listener.Close()
	defer os.Remove(s.socket)
	if err := os.Chmod(s.socket, 0o600); err != nil {
		return fmt.Errorf("secure socket: %w", err)
	}

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		go s.handle(connection)
	}
}

func (s *Server) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	var request Request
	if err := json.NewDecoder(connection).Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(Response{OK: false, Message: "invalid request: " + err.Error()})
		return
	}
	response, shutdown := s.dispatch(request)
	if err := json.NewEncoder(connection).Encode(response); err != nil {
		s.logger.Printf("write control response: %v", err)
	}
	if shutdown {
		s.cancel()
	}
}

func (s *Server) dispatch(request Request) (Response, bool) {
	switch request.Action {
	case "status":
		s.mu.RLock()
		defer s.mu.RUnlock()
		manager := s.manager
		statuses, err := manager.Status(request.Names)
		if err == nil {
			supervisor.CollectMetrics(statuses)
		}
		return result(statuses, err), false
	case "start":
		s.mu.RLock()
		defer s.mu.RUnlock()
		return resultMessage("started "+targets(request.Names), s.manager.Start(request.Names)), false
	case "stop":
		s.mu.RLock()
		defer s.mu.RUnlock()
		return resultMessage("stopped "+targets(request.Names), s.manager.Stop(request.Names)), false
	case "restart":
		s.mu.RLock()
		defer s.mu.RUnlock()
		return resultMessage("restarted "+targets(request.Names), s.manager.Restart(request.Names)), false
	case "pause":
		s.mu.RLock()
		defer s.mu.RUnlock()
		return resultMessage("paused "+targets(request.Names), s.manager.Pause(request.Names)), false
	case "resume":
		s.mu.RLock()
		defer s.mu.RUnlock()
		return resultMessage("resumed "+targets(request.Names), s.manager.Resume(request.Names)), false
	case "disable":
		s.mu.RLock()
		defer s.mu.RUnlock()
		return resultMessage("disabled "+targets(request.Names), s.manager.Disable(request.Names)), false
	case "enable":
		s.mu.RLock()
		defer s.mu.RUnlock()
		return resultMessage("enabled "+targets(request.Names), s.manager.Enable(request.Names)), false
	case "reload":
		return s.reload(), false
	case "shutdown", "shundown":
		return Response{OK: true, Message: "daemon is shutting down"}, true
	default:
		return Response{OK: false, Message: fmt.Sprintf("unknown action %q", request.Action)}, false
	}
}

func (s *Server) Execute(request Request) Response {
	response, _ := s.dispatch(request)
	return response
}

func (s *Server) Events(after uint64, limit int) []supervisor.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.manager.Events(after, limit)
}

func (s *Server) ConfigPath() string {
	return s.configPath
}

func (s *Server) reload() Response {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return Response{OK: false, Message: err.Error()}
	}
	config.ResolvePaths(&cfg, s.configPath)
	if cfg.Socket != s.runtime.Socket || cfg.StateDir != s.runtime.StateDir || cfg.EventHistory != s.runtime.EventHistory || cfg.Web != s.runtime.Web {
		return Response{OK: false, Message: "socket, web, state_dir, and event_history changes require a daemon restart"}
	}

	s.mu.Lock()
	current := s.manager
	if err := current.Apply(cfg.Programs); err != nil {
		s.mu.Unlock()
		return Response{OK: false, Message: "apply configuration: " + err.Error()}
	}
	s.mu.Unlock()
	return Response{OK: true, Message: "configuration reloaded"}
}

func (s *Server) Manager() *supervisor.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.manager
}

func result(statuses []supervisor.Status, err error) Response {
	if err != nil {
		return Response{OK: false, Message: err.Error()}
	}
	return Response{OK: true, Processes: statuses}
}

func resultMessage(message string, err error) Response {
	if err != nil {
		return Response{OK: false, Message: err.Error()}
	}
	return Response{OK: true, Message: message}
}

func targets(names []string) string {
	if len(names) == 0 {
		return "all"
	}
	return strings.Join(names, ", ")
}

func prepareSocket(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect socket: %w", err)
	}
	connection, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		connection.Close()
		return fmt.Errorf("daemon is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

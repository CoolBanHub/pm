package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/control"
	"github.com/CoolBanHub/pm/internal/supervisor"

	"gopkg.in/yaml.v3"
)

//go:embed static/*
var staticFiles embed.FS

type Backend interface {
	Execute(control.Request) control.Response
	Events(after uint64, limit int) []supervisor.Event
	ConfigPath() string
}

type Server struct {
	listen   string
	token    string
	backend  Backend
	logger   *log.Logger
	http     *http.Server
	configMu sync.Mutex
}

func NewServer(listen, token string, backend Backend, logger *log.Logger) *Server {
	server := &Server{listen: listen, token: token, backend: backend, logger: logger}
	server.http = &http.Server{
		Addr:              listen,
		Handler:           server.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return server
}

func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen on web address %s: %w", s.listen, err)
	}
	s.logger.Printf("web administration listening on http://%s", displayAddress(listener.Addr().String()))
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
	}()
	if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/session", s.api(s.handleSession))
	mux.HandleFunc("GET /api/v1/processes", s.api(s.handleProcesses))
	mux.HandleFunc("GET /api/v1/processes/{name}/config", s.api(s.handleGetProcessConfig))
	mux.HandleFunc("POST /api/v1/processes", s.api(s.handleCreateProcess))
	mux.HandleFunc("PUT /api/v1/processes/{name}", s.api(s.handleUpdateProcess))
	mux.HandleFunc("DELETE /api/v1/processes/{name}", s.api(s.handleDeleteProcess))
	mux.HandleFunc("POST /api/v1/processes/{name}/{action}", s.api(s.handleProcessAction))
	mux.HandleFunc("POST /api/v1/actions/{action}", s.api(s.handleBulkAction))
	mux.HandleFunc("POST /api/v1/groups/{group}/{action}", s.api(s.handleGroupAction))
	mux.HandleFunc("GET /api/v1/events", s.api(s.handleEvents))
	mux.HandleFunc("GET /api/v1/logs/{name}", s.api(s.handleLog))
	mux.HandleFunc("GET /api/v1/logs/{name}/stream", s.api(s.handleLogStream))
	mux.HandleFunc("GET /api/v1/config", s.api(s.handleGetConfig))
	mux.HandleFunc("POST /api/v1/config/validate", s.api(s.handleValidateConfig))
	mux.HandleFunc("PUT /api/v1/config", s.api(s.handlePutConfig))

	assets, _ := fs.Sub(staticFiles, "static")
	files := http.FileServer(http.FS(assets))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path != "/" {
			if _, err := fs.Stat(assets, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	}))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	response := s.backend.Execute(control.Request{Action: "status"})
	if !response.OK {
		writeError(w, http.StatusServiceUnavailable, response.Message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) api(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.token != "" {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if len(provided) != len(s.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "invalid access token")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "request origin does not match this server")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleSession(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "token_required": s.token != ""})
}

func (s *Server) handleProcesses(w http.ResponseWriter, _ *http.Request) {
	response := s.backend.Execute(control.Request{Action: "status"})
	if !response.OK {
		writeError(w, http.StatusInternalServerError, response.Message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"processes": response.Processes, "timestamp": time.Now()})
}

func (s *Server) handleProcessAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "start" && action != "stop" && action != "restart" && action != "pause" && action != "resume" {
		writeError(w, http.StatusNotFound, "unknown process action")
		return
	}
	response := s.backend.Execute(control.Request{Action: action, Names: []string{r.PathValue("name")}})
	writeControlResponse(w, response)
}

func (s *Server) handleCreateProcess(w http.ResponseWriter, r *http.Request) {
	program := config.DefaultProgram()
	if err := decodeBody(r, &program); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	err := s.updatePrograms(func(programs []config.Program) ([]config.Program, error) {
		for _, existing := range programs {
			if existing.Name == program.Name {
				return nil, fmt.Errorf("program %q already exists", program.Name)
			}
		}
		return append(programs, program), nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"message": "process created", "program": program})
}

func (s *Server) handleGetProcessConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadOrDefault(s.backend.ConfigPath())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, program := range cfg.Programs {
		if program.Name == r.PathValue("name") {
			writeJSON(w, http.StatusOK, map[string]any{"program": program})
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("unknown program %q", r.PathValue("name")))
}

func (s *Server) handleUpdateProcess(w http.ResponseWriter, r *http.Request) {
	program := config.DefaultProgram()
	if err := decodeBody(r, &program); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	originalName := r.PathValue("name")
	err := s.updatePrograms(func(programs []config.Program) ([]config.Program, error) {
		position := -1
		for i, existing := range programs {
			if existing.Name == originalName {
				position = i
			}
			if existing.Name == program.Name && existing.Name != originalName {
				return nil, fmt.Errorf("program %q already exists", program.Name)
			}
		}
		if position < 0 {
			return nil, fmt.Errorf("unknown program %q", originalName)
		}
		programs[position] = program
		return programs, nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "process updated", "program": program})
}

func (s *Server) handleDeleteProcess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	status := s.backend.Execute(control.Request{Action: "status", Names: []string{name}})
	if !status.OK || len(status.Processes) != 1 || status.Processes[0].Name != name {
		message := status.Message
		if message == "" {
			message = fmt.Sprintf("unknown program %q", name)
		}
		writeError(w, http.StatusNotFound, message)
		return
	}
	if !status.Processes[0].Paused {
		writeError(w, http.StatusConflict, fmt.Sprintf("program %q must be paused before deletion", name))
		return
	}
	err := s.updatePrograms(func(programs []config.Program) ([]config.Program, error) {
		result := make([]config.Program, 0, len(programs))
		found := false
		for _, program := range programs {
			if program.Name == name {
				found = true
				continue
			}
			result = append(result, program)
		}
		if !found {
			return nil, fmt.Errorf("unknown program %q", name)
		}
		return result, nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "process deleted"})
}

func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "start" && action != "stop" && action != "restart" && action != "pause" && action != "resume" && action != "reload" {
		writeError(w, http.StatusNotFound, "unknown bulk action")
		return
	}
	names := []string{"all"}
	if action == "reload" {
		names = nil
	} else if r.ContentLength != 0 {
		var request struct {
			Names []string `json:"names"`
		}
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(request.Names) == 0 {
			writeError(w, http.StatusBadRequest, "at least one process must be selected")
			return
		}
		names = request.Names
	}
	response := s.backend.Execute(control.Request{Action: action, Names: names})
	writeControlResponse(w, response)
}

func (s *Server) handleGroupAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "start" && action != "stop" && action != "restart" {
		writeError(w, http.StatusNotFound, "unknown group action")
		return
	}
	status := s.backend.Execute(control.Request{Action: "status"})
	if !status.OK {
		writeError(w, http.StatusInternalServerError, status.Message)
		return
	}
	group := r.PathValue("group")
	names := make([]string, 0)
	for _, process := range status.Processes {
		if process.Group == group {
			names = append(names, process.Name)
		}
	}
	if len(names) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown or empty group %q", group))
		return
	}
	writeControlResponse(w, s.backend.Execute(control.Request{Action: action, Names: names}))
}

func (s *Server) updatePrograms(mutate func([]config.Program) ([]config.Program, error)) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg, err := config.LoadOrDefault(s.backend.ConfigPath())
	if err != nil {
		return err
	}
	cfg.Programs, err = mutate(cfg.Programs)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(cfg); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	content := append(buffer.Bytes(), '\n')
	if err := writeConfigAtomic(s.backend.ConfigPath(), content); err != nil {
		return err
	}
	response := s.backend.Execute(control.Request{Action: "reload"})
	if !response.OK {
		return errors.New(response.Message)
	}
	return nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": s.backend.Events(after, limit)})
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	path, err := s.logPath(r.PathValue("name"), r.URL.Query().Get("stream"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if lines <= 0 || lines > 5000 {
		lines = 300
	}
	content, err := readLastLines(path, lines)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": string(content), "path": path})
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	path, err := s.logPath(r.PathValue("name"), r.URL.Query().Get("stream"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	lines, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if lines <= 0 || lines > 5000 {
		lines = 300
	}
	initial, _ := readLastLines(path, lines)
	if len(initial) > 0 {
		initial = append(initial, '\n')
	}
	writeSSE(w, "chunk", string(initial))
	flusher.Flush()

	var file *os.File
	var previous os.FileInfo
	openAtEnd := func(fromStart bool) {
		if file != nil {
			_ = file.Close()
		}
		file, _ = os.Open(path)
		if file != nil {
			previous, _ = file.Stat()
			if !fromStart {
				_, _ = file.Seek(0, io.SeekEnd)
			}
		}
	}
	openAtEnd(false)
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	buffer := make([]byte, 32*1024)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			current, statErr := os.Stat(path)
			if statErr == nil && (file == nil || previous == nil || !os.SameFile(previous, current)) {
				openAtEnd(true)
			}
			if file == nil {
				continue
			}
			for {
				n, readErr := file.Read(buffer)
				if n > 0 {
					writeSSE(w, "chunk", string(buffer[:n]))
					flusher.Flush()
				}
				if readErr != nil || n == 0 {
					break
				}
			}
		}
	}
}

func (s *Server) logPath(name, stream string) (string, error) {
	response := s.backend.Execute(control.Request{Action: "status", Names: []string{name}})
	if !response.OK || len(response.Processes) != 1 {
		return "", errors.New(response.Message)
	}
	path := response.Processes[0].StdoutLog
	if stream == "stderr" {
		path = response.Processes[0].StderrLog
	}
	if path == "" {
		return "", errors.New("selected log file is not configured")
	}
	return path, nil
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	content, err := os.ReadFile(s.backend.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		content, err = yaml.Marshal(config.DefaultConfig())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": string(content), "path": s.backend.ConfigPath()})
}

func (s *Server) handleValidateConfig(w http.ResponseWriter, r *http.Request) {
	content, err := configContent(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := config.Parse([]byte(content)); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	var request struct {
		Content string `json:"content"`
		Apply   bool   `json:"apply"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next, err := config.Parse([]byte(request.Content))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	current, err := config.LoadOrDefault(s.backend.ConfigPath())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	config.ResolvePaths(&next, s.backend.ConfigPath())
	config.ResolvePaths(&current, s.backend.ConfigPath())
	restartRequired := next.Socket != current.Socket || next.StateDir != current.StateDir || next.EventHistory != current.EventHistory || next.Web != current.Web
	if err := writeConfigAtomic(s.backend.ConfigPath(), []byte(request.Content)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	message := "configuration saved"
	if request.Apply && !restartRequired {
		response := s.backend.Execute(control.Request{Action: "reload"})
		if !response.OK {
			writeError(w, http.StatusInternalServerError, response.Message)
			return
		}
		message = response.Message
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": message, "restart_required": restartRequired})
}

func configContent(r *http.Request) (string, error) {
	var request struct {
		Content string `json:"content"`
	}
	if err := decodeBody(r, &request); err != nil {
		return "", err
	}
	return request.Content, nil
}

func decodeBody(r *http.Request, target any) error {
	const maximum = 2 << 20
	data, err := io.ReadAll(io.LimitReader(r.Body, maximum+1))
	if err != nil {
		return err
	}
	if len(data) > maximum {
		return errors.New("request body exceeds 2 MB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writeConfigAtomic(path string, content []byte) error {
	mode := os.FileMode(0o644)
	info, err := os.Stat(path)
	if err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if old, readErr := os.ReadFile(path); readErr == nil {
		if err := os.WriteFile(path+".bak", old, mode); err != nil {
			return fmt.Errorf("write config backup: %w", err)
		}
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pm-config-*")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func readLastLines(path string, lines int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size()
	data := make([]byte, 0, 4096)
	for offset > 0 && bytes.Count(data, []byte{'\n'}) <= lines {
		chunkSize := int64(4096)
		if offset < chunkSize {
			chunkSize = offset
		}
		offset -= chunkSize
		chunk := make([]byte, chunkSize)
		_, _ = file.ReadAt(chunk, offset)
		data = append(chunk, data...)
	}
	data = bytes.TrimSuffix(data, []byte{'\n'})
	parts := bytes.Split(data, []byte{'\n'})
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return bytes.Join(parts, []byte{'\n'}), nil
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host == r.Host && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func writeControlResponse(w http.ResponseWriter, response control.Response) {
	if !response.OK {
		writeError(w, http.StatusConflict, response.Message)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func writeSSE(w io.Writer, event string, value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func displayAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	if host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultFile   = "pm.yaml"
	DefaultSocket = "/tmp/pm.sock"
)

type Config struct {
	Socket       string    `json:"socket" yaml:"socket"`
	StateDir     string    `json:"state_dir" yaml:"state_dir"`
	EventHistory int       `json:"event_history" yaml:"event_history"`
	Web          Web       `json:"web" yaml:"web"`
	Programs     []Program `json:"programs" yaml:"programs"`
}

type Web struct {
	Enabled  bool   `json:"enabled" yaml:"enabled"`
	Listen   string `json:"listen" yaml:"listen"`
	Token    string `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
}

type Program struct {
	Name          string            `json:"name" yaml:"name"`
	Group         string            `json:"group" yaml:"group"`
	Command       string            `json:"command" yaml:"command"`
	Args          []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Directory     string            `json:"directory,omitempty" yaml:"directory,omitempty"`
	Environment   map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`
	Autostart     bool              `json:"autostart" yaml:"autostart"`
	Restart       string            `json:"restart" yaml:"restart"`
	RestartDelay  string            `json:"restart_delay" yaml:"restart_delay"`
	MaxRestarts   int               `json:"max_restarts" yaml:"max_restarts"`
	RestartWindow string            `json:"restart_window" yaml:"restart_window"`
	StopSignal    string            `json:"stop_signal" yaml:"stop_signal"`
	StopTimeout   string            `json:"stop_timeout" yaml:"stop_timeout"`
	StdoutLog     string            `json:"stdout_log,omitempty" yaml:"stdout_log,omitempty"`
	StderrLog     string            `json:"stderr_log,omitempty" yaml:"stderr_log,omitempty"`
	LogMaxBytes   int64             `json:"log_max_bytes" yaml:"log_max_bytes"`
	LogBackups    int               `json:"log_backups" yaml:"log_backups"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

func LoadOrDefault(path string) (Config, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	return Config{}, err
}

func DefaultConfig() Config {
	return Config{
		Socket:       DefaultSocket,
		StateDir:     ".pm",
		EventHistory: 1000,
		Web: Web{
			Enabled: true,
			Listen:  "127.0.0.1:19090",
		},
		Programs: []Program{},
	}
}

func Parse(data []byte) (Config, error) {
	var raw struct {
		Socket       string      `yaml:"socket"`
		StateDir     string      `yaml:"state_dir"`
		EventHistory int         `yaml:"event_history"`
		Web          Web         `yaml:"web"`
		Programs     []yaml.Node `yaml:"programs"`
	}
	defaults := DefaultConfig()
	raw.Socket = defaults.Socket
	raw.StateDir = defaults.StateDir
	raw.EventHistory = defaults.EventHistory
	raw.Web = defaults.Web
	if err := decodeYAML(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	cfg := Config{
		Socket:       raw.Socket,
		StateDir:     raw.StateDir,
		EventHistory: raw.EventHistory,
		Web:          raw.Web,
		Programs:     make([]Program, 0, len(raw.Programs)),
	}
	// Programs are decoded individually so each entry picks up DefaultProgram
	// values for omitted fields and is strict-checked for unknown keys.
	for i, node := range raw.Programs {
		item, err := yaml.Marshal(&node)
		if err != nil {
			return Config{}, fmt.Errorf("parse program %d: %w", i, err)
		}
		program := DefaultProgram()
		if err := decodeYAML(item, &program); err != nil {
			return Config{}, fmt.Errorf("parse program %d: %w", i, err)
		}
		cfg.Programs = append(cfg.Programs, program)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func DefaultProgram() Program {
	return Program{
		Group:         "default",
		Autostart:     true,
		Restart:       "unexpected",
		RestartDelay:  "1s",
		MaxRestarts:   5,
		RestartWindow: "1m",
		StopSignal:    "TERM",
		StopTimeout:   "10s",
		LogMaxBytes:   10 * 1024 * 1024,
		LogBackups:    3,
	}
}

func decodeYAML(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple YAML documents are not allowed")
		}
		return err
	}
	return nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Socket) == "" {
		return errors.New("socket cannot be empty")
	}
	if strings.TrimSpace(c.StateDir) == "" {
		return errors.New("state_dir cannot be empty")
	}
	if c.EventHistory < 0 {
		return errors.New("event_history cannot be negative")
	}
	if c.Web.Enabled {
		host, _, err := net.SplitHostPort(c.Web.Listen)
		if err != nil {
			return fmt.Errorf("web.listen: %w", err)
		}
		if !isLoopback(host) && c.Web.Token == "" && c.Web.TokenEnv == "" {
			return errors.New("web token or token_env is required when listening beyond localhost")
		}
	}
	names := make(map[string]struct{}, len(c.Programs))
	for i, p := range c.Programs {
		prefix := fmt.Sprintf("programs[%d]", i)
		if p.Name == "" {
			return fmt.Errorf("%s.name is required", prefix)
		}
		if strings.ContainsAny(p.Name, " \t\r\n") {
			return fmt.Errorf("%s.name cannot contain whitespace", prefix)
		}
		if strings.TrimSpace(p.Group) == "" {
			return fmt.Errorf("%s.group cannot be empty", prefix)
		}
		if len(p.Group) > 64 || strings.ContainsAny(p.Group, "\r\n\t") {
			return fmt.Errorf("%s.group must be at most 64 characters without control whitespace", prefix)
		}
		if _, exists := names[p.Name]; exists {
			return fmt.Errorf("duplicate program name %q", p.Name)
		}
		names[p.Name] = struct{}{}
		if p.Command == "" {
			return fmt.Errorf("%s.command is required", prefix)
		}
		switch p.Restart {
		case "never", "unexpected", "always":
		default:
			return fmt.Errorf("%s.restart must be never, unexpected, or always", prefix)
		}
		if _, err := p.RestartDelayDuration(); err != nil {
			return fmt.Errorf("%s.restart_delay: %w", prefix, err)
		}
		if _, err := p.RestartWindowDuration(); err != nil {
			return fmt.Errorf("%s.restart_window: %w", prefix, err)
		}
		if _, err := p.StopTimeoutDuration(); err != nil {
			return fmt.Errorf("%s.stop_timeout: %w", prefix, err)
		}
		if p.MaxRestarts < 0 {
			return fmt.Errorf("%s.max_restarts cannot be negative", prefix)
		}
		if p.LogMaxBytes < 0 {
			return fmt.Errorf("%s.log_max_bytes cannot be negative", prefix)
		}
		if p.LogBackups < 0 {
			return fmt.Errorf("%s.log_backups cannot be negative", prefix)
		}
		switch strings.ToUpper(p.StopSignal) {
		case "TERM", "INT", "QUIT", "HUP":
		default:
			return fmt.Errorf("%s.stop_signal must be TERM, INT, QUIT, or HUP", prefix)
		}
	}
	return nil
}

func (p Program) RestartDelayDuration() (time.Duration, error) {
	return positiveDuration(p.RestartDelay)
}

func (p Program) RestartWindowDuration() (time.Duration, error) {
	return positiveDuration(p.RestartWindow)
}

func (p Program) StopTimeoutDuration() (time.Duration, error) {
	return positiveDuration(p.StopTimeout)
}

func positiveDuration(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("must be greater than zero")
	}
	return d, nil
}

func ResolvePaths(cfg *Config, configPath string) {
	base, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		base = filepath.Dir(configPath)
	}
	cfg.Socket = resolve(base, cfg.Socket)
	cfg.StateDir = resolve(base, cfg.StateDir)
	for i := range cfg.Programs {
		p := &cfg.Programs[i]
		p.Directory = resolve(base, p.Directory)
		p.StdoutLog = resolve(base, p.StdoutLog)
		p.StderrLog = resolve(base, p.StderrLog)
	}
}

func (w Web) ResolvedToken() (string, error) {
	if w.Token != "" {
		return w.Token, nil
	}
	if w.TokenEnv == "" {
		return "", nil
	}
	token := os.Getenv(w.TokenEnv)
	if token == "" {
		return "", fmt.Errorf("environment variable %s is empty", w.TokenEnv)
	}
	return token, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func resolve(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	DefaultFile = "pm.yaml"
	// DefaultPriority matches supervisor's priority default: lower values start
	// first and stop last.
	DefaultPriority = 999
)

// defaultConfigTemplate is written to ~/.pm/pm.yaml on first run so the
// default configuration is visible and editable. See SeedDefaultConfig.
//
//go:embed default_config.yaml
var defaultConfigTemplate []byte

// HomeDir returns PM's default data directory (~/.pm). All default config,
// logs, state, and the control socket live beneath it. It falls back to a
// relative ".pm" when the user home cannot be determined.
func HomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".pm"
	}
	return filepath.Join(home, ".pm")
}

// DefaultConfigPath returns the default configuration file path (~/.pm/pm.yaml).
func DefaultConfigPath() string {
	return filepath.Join(HomeDir(), DefaultFile)
}

// DefaultSocketPath returns the default control socket path (~/.pm/pm.sock),
// used as an absolute fallback when no configuration can be loaded.
func DefaultSocketPath() string {
	return filepath.Join(HomeDir(), "pm.sock")
}

type Config struct {
	Socket       string `json:"socket" yaml:"socket"`
	StateDir     string `json:"state_dir" yaml:"state_dir"`
	EventHistory int    `json:"event_history" yaml:"event_history"`
	Web          Web    `json:"web" yaml:"web"`
	// Include lists glob patterns of drop-in files that contribute additional
	// programs (supervisor's [include] equivalent). Patterns are relative to
	// the including file's directory; matched files may only define programs
	// and further includes. Daemon-level fields stay in the main file.
	Include  []string  `json:"include,omitempty" yaml:"include,omitempty"`
	Programs []Program `json:"programs" yaml:"programs"`
}

type Web struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Listen  string `json:"listen" yaml:"listen"`
	// Username and Password enable browser login for the web console. Both
	// must be set together; when unset the console is only reachable without
	// authentication on loopback listeners.
	Username string `json:"username,omitempty" yaml:"username,omitempty"`
	Password string `json:"password,omitempty" yaml:"password,omitempty"`
	// Token and TokenEnv are deprecated and no longer authenticate anything.
	// The fields remain so older configuration files still parse; setting
	// either fails validation with a migration hint.
	Token    string `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
}

type Program struct {
	Name        string            `json:"name" yaml:"name"`
	Group       string            `json:"group" yaml:"group"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Command     string            `json:"command" yaml:"command"`
	Args        []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Directory   string            `json:"directory,omitempty" yaml:"directory,omitempty"`
	Environment map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`
	Autostart   bool              `json:"autostart" yaml:"autostart"`
	Paused      bool              `json:"paused,omitempty" yaml:"-"`
	Disabled    bool              `json:"disabled,omitempty" yaml:"-"`
	// Priority orders startup (ascending) and shutdown (descending) across
	// programs, mirroring supervisor's priority. Equal priorities fall back to
	// name order.
	Priority     int    `json:"priority" yaml:"priority"`
	Restart      string `json:"restart" yaml:"restart"`
	RestartDelay string `json:"restart_delay" yaml:"restart_delay"`
	MaxRestarts  int    `json:"max_restarts" yaml:"max_restarts"`
	// RestartWindow is the sliding window restarts are counted against.
	RestartWindow string `json:"restart_window" yaml:"restart_window"`
	// ExitCodes lists exit codes considered expected under the "unexpected"
	// restart policy. Empty means only 0 is expected. An exit before the
	// start_secs window elapses is always a start failure regardless of code.
	ExitCodes []int `json:"exit_codes,omitempty" yaml:"exit_codes,omitempty"`
	// StartSecs is how long a freshly started process must survive before it
	// counts as RUNNING. "0s" disables the STARTING phase.
	StartSecs   string `json:"start_secs" yaml:"start_secs"`
	StopSignal  string `json:"stop_signal" yaml:"stop_signal"`
	StopTimeout string `json:"stop_timeout" yaml:"stop_timeout"`
	StdoutLog   string `json:"stdout_log,omitempty" yaml:"stdout_log,omitempty"`
	StderrLog   string `json:"stderr_log,omitempty" yaml:"stderr_log,omitempty"`
	LogMaxBytes int64  `json:"log_max_bytes" yaml:"log_max_bytes"`
	LogBackups  int    `json:"log_backups" yaml:"log_backups"`
	PprofURL    string `json:"pprof_url,omitempty" yaml:"pprof_url,omitempty"`
	// Source is the absolute path of the file that defined this program: the
	// main config, or a matched include file. It is metadata for tooling (the
	// web editor refuses to rewrite programs owned by include files) and never
	// affects process runtime. Empty when parsed from anonymous content.
	Source string `json:"source,omitempty" yaml:"-"`
}

func Load(path string) (Config, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := loadMainFile(absolute)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadMainFile reads the main configuration file and merges the programs of
// every include match. Daemon-level fields come from the main file only.
func loadMainFile(path string) (Config, error) {
	programs, raw, err := parseFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Socket:       raw.Socket,
		StateDir:     raw.StateDir,
		EventHistory: raw.EventHistory,
		Web:          raw.Web,
		Include:      raw.Include,
		Programs:     programs,
	}
	stampSource(cfg.Programs, path)
	seen := make(map[string]string, len(cfg.Programs))
	for _, program := range cfg.Programs {
		seen[program.Name] = path
	}
	for _, pattern := range raw.Include {
		matches, err := expandInclude(filepath.Dir(path), pattern)
		if err != nil {
			return Config{}, fmt.Errorf("%s: include %q: %w", path, pattern, err)
		}
		for _, match := range matches {
			included, err := loadIncludedFile(match, map[string]struct{}{path: {}}, seen)
			if err != nil {
				return Config{}, err
			}
			cfg.Programs = append(cfg.Programs, included...)
		}
	}
	return cfg, nil
}

// loadIncludedFile reads one drop-in file: programs plus optional nested
// includes. chain guards against include cycles per branch. seen carries
// name→source across the whole load for duplicate detection with file context.
func loadIncludedFile(path string, chain map[string]struct{}, seen map[string]string) ([]Program, error) {
	if _, cycling := chain[path]; cycling {
		return nil, fmt.Errorf("include cycle through %s", path)
	}
	chain[path] = struct{}{}
	programs, includes, err := parseIncludeFile(path)
	if err != nil {
		return nil, err
	}
	stampSource(programs, path)
	for _, program := range programs {
		if previous, duplicate := seen[program.Name]; duplicate {
			return nil, fmt.Errorf("duplicate program %q defined in both %s and %s", program.Name, previous, path)
		}
		seen[program.Name] = path
	}
	for _, pattern := range includes {
		matches, err := expandInclude(filepath.Dir(path), pattern)
		if err != nil {
			return nil, fmt.Errorf("%s: include %q: %w", path, pattern, err)
		}
		for _, match := range matches {
			nested, err := loadIncludedFile(match, chain, seen)
			if err != nil {
				return nil, err
			}
			programs = append(programs, nested...)
		}
	}
	return programs, nil
}

// expandInclude resolves one include pattern against base. Absolute patterns
// are used verbatim; relative patterns join base. A pattern matching nothing
// is not an error so an empty conf.d is valid.
func expandInclude(base, pattern string) ([]string, error) {
	pattern = expandEnv(pattern)
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("pattern cannot be empty")
	}
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(base, pattern)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// fileContent is the raw shape of the main configuration file. Include files
// are decoded as includeContent instead so daemon-level fields fail strict
// decoding there.
type fileContent struct {
	Socket       string      `yaml:"socket"`
	StateDir     string      `yaml:"state_dir"`
	EventHistory int         `yaml:"event_history"`
	Web          Web         `yaml:"web"`
	Include      []string    `yaml:"include"`
	Programs     []yaml.Node `yaml:"programs"`
}

// includeContent is all a drop-in file may define: programs and further
// includes. Anything else (socket, web, ...) is rejected by KnownFields.
type includeContent struct {
	Include  []string    `yaml:"include"`
	Programs []yaml.Node `yaml:"programs"`
}

// parseIncludeFile reads one drop-in file from disk.
func parseIncludeFile(path string) ([]Program, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	var raw includeContent
	if err := decodeYAML(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("%s: drop-in files only support programs and include: %w", path, err)
	}
	programs, err := decodeProgramNodes(raw.Programs)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return programs, raw.Include, nil
}

// parseFile reads and decodes the main configuration YAML file from disk.
func parseFile(path string) ([]Program, fileContent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fileContent{}, fmt.Errorf("read config: %w", err)
	}
	programs, raw, err := parseContent(data)
	if err != nil {
		return nil, fileContent{}, fmt.Errorf("%s: %w", path, err)
	}
	return programs, raw, nil
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

// SeedDefaultConfig writes the default configuration template to path when it
// does not yet exist, creating the parent directory. It makes the default
// configuration under ~/.pm tangible and editable on first run. If the file
// already exists it is left untouched.
func SeedDefaultConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, defaultConfigTemplate, 0o600)
}

// DefaultConfig returns built-in defaults. Socket and StateDir are relative so
// they resolve against the configuration file's directory (~/.pm by default),
// keeping all default data in one place.
func DefaultConfig() Config {
	return Config{
		Socket:       "pm.sock",
		StateDir:     ".",
		EventHistory: 1000,
		Web: Web{
			Enabled: true,
			Listen:  "127.0.0.1:19090",
		},
		Programs: []Program{},
	}
}

// Parse decodes configuration content without touching the filesystem:
// include patterns are recorded but not followed (callers validating edited
// content cannot resolve them; Load follows includes for real merges).
func Parse(data []byte) (Config, error) {
	programs, raw, err := parseContent(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg := Config{
		Socket:       raw.Socket,
		StateDir:     raw.StateDir,
		EventHistory: raw.EventHistory,
		Web:          raw.Web,
		Include:      raw.Include,
		Programs:     programs,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseContent(data []byte) ([]Program, fileContent, error) {
	var raw fileContent
	defaults := DefaultConfig()
	raw.Socket = defaults.Socket
	raw.StateDir = defaults.StateDir
	raw.EventHistory = defaults.EventHistory
	raw.Web = defaults.Web
	if err := decodeYAML(data, &raw); err != nil {
		return nil, fileContent{}, err
	}
	programs, err := decodeProgramNodes(raw.Programs)
	if err != nil {
		return nil, fileContent{}, err
	}
	return programs, raw, nil
}

// decodeProgramNodes decodes each programs entry individually so omitted
// fields pick up DefaultProgram values and unknown keys are rejected.
func decodeProgramNodes(nodes []yaml.Node) ([]Program, error) {
	programs := make([]Program, 0, len(nodes))
	for i, node := range nodes {
		item, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("parse program %d: %w", i, err)
		}
		program := DefaultProgram()
		if err := decodeYAML(item, &program); err != nil {
			return nil, fmt.Errorf("parse program %d: %w", i, err)
		}
		expandProgramEnv(&program)
		programs = append(programs, program)
	}
	return programs, nil
}

func stampSource(programs []Program, path string) {
	for i := range programs {
		programs[i].Source = path
	}
}

func DefaultProgram() Program {
	return Program{
		Group:         "default",
		Autostart:     true,
		Priority:      DefaultPriority,
		Restart:       "unexpected",
		RestartDelay:  "1s",
		MaxRestarts:   5,
		RestartWindow: "1m",
		StartSecs:     "0s",
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
	for _, pattern := range c.Include {
		if strings.TrimSpace(pattern) == "" {
			return errors.New("include patterns cannot be empty")
		}
	}
	if c.Web.Enabled {
		host, _, err := net.SplitHostPort(c.Web.Listen)
		if err != nil {
			return fmt.Errorf("web.listen: %w", err)
		}
		if c.Web.Token != "" || c.Web.TokenEnv != "" {
			return errors.New("web.token/token_env are no longer supported; configure web.username/web.password instead and remove the deprecated fields")
		}
		if (c.Web.Username == "") != (c.Web.Password == "") {
			return errors.New("web.username and web.password must be configured together")
		}
		if !isLoopback(host) && c.Web.Username == "" {
			return errors.New("web username and password are required when listening beyond localhost")
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
		if strings.ContainsAny(p.Description, "\r\n\t") {
			return fmt.Errorf("%s.description cannot contain newlines or tabs", prefix)
		}
		if utf8.RuneCountInString(p.Description) > 256 {
			return fmt.Errorf("%s.description must be at most 256 characters", prefix)
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
		if _, err := p.StartSecsDuration(); err != nil {
			return fmt.Errorf("%s.start_secs: %w", prefix, err)
		}
		for _, code := range p.ExitCodes {
			if code < -1 || code > 255 {
				return fmt.Errorf("%s.exit_codes values must be between -1 and 255", prefix)
			}
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
		if p.PprofURL != "" {
			parsed, err := url.ParseRequestURI(p.PprofURL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return fmt.Errorf("%s.pprof_url must be an HTTP(S) pprof base URL without query or fragment", prefix)
			}
		}
		switch strings.ToUpper(p.StopSignal) {
		case "TERM", "INT", "QUIT", "HUP", "USR1", "USR2":
		default:
			return fmt.Errorf("%s.stop_signal must be TERM, INT, QUIT, HUP, USR1, or USR2", prefix)
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

func (p Program) StartSecsDuration() (time.Duration, error) {
	d, err := time.ParseDuration(p.StartSecs)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, errors.New("cannot be negative")
	}
	return d, nil
}

func (p Program) StopTimeoutDuration() (time.Duration, error) {
	return positiveDuration(p.StopTimeout)
}

// ExpectedExit reports whether exitCode is an expected exit under the
// "unexpected" restart policy. An empty ExitCodes means only 0 is expected.
func (p Program) ExpectedExit(exitCode int) bool {
	if len(p.ExitCodes) == 0 {
		return exitCode == 0
	}
	for _, code := range p.ExitCodes {
		if code == exitCode {
			return true
		}
	}
	return false
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

// envExpansion matches only braced references (${NAME}) so bare dollar signs
// in passwords or shell snippets pass through untouched.
var envExpansion = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv substitutes ${NAME} with the daemon's environment variable NAME
// (empty when unset). It mirrors supervisor's %(ENV_NAME)s substitution.
func expandEnv(value string) string {
	if !strings.Contains(value, "${") {
		return value
	}
	return envExpansion.ReplaceAllStringFunc(value, func(match string) string {
		return os.Getenv(match[2 : len(match)-1])
	})
}

// expandProgramEnv applies ${NAME} expansion to the path- and command-like
// fields where deployment-specific values are common.
func expandProgramEnv(p *Program) {
	p.Command = expandEnv(p.Command)
	for i, arg := range p.Args {
		p.Args[i] = expandEnv(arg)
	}
	p.Directory = expandEnv(p.Directory)
	for key, value := range p.Environment {
		p.Environment[key] = expandEnv(value)
	}
	p.StdoutLog = expandEnv(p.StdoutLog)
	p.StderrLog = expandEnv(p.StderrLog)
	p.PprofURL = expandEnv(p.PprofURL)
}

// ResolvePaths anchors relative paths. Socket and state_dir resolve against
// the main config file's directory; each program's paths resolve against the
// directory of the file that defined it, so drop-in files stay relocatable.
func ResolvePaths(cfg *Config, configPath string) {
	base, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		base = filepath.Dir(configPath)
	}
	cfg.Socket = resolve(base, cfg.Socket)
	cfg.StateDir = resolve(base, cfg.StateDir)
	for i := range cfg.Programs {
		programBase := base
		if cfg.Programs[i].Source != "" {
			programBase = filepath.Dir(cfg.Programs[i].Source)
		}
		p := &cfg.Programs[i]
		p.Directory = resolve(programBase, p.Directory)
		p.StdoutLog = resolve(programBase, p.StdoutLog)
		p.StderrLog = resolve(programBase, p.StderrLog)
	}
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

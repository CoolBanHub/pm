package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadDefaultsAndResolvePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pm.yaml")
	data := []byte("socket: run/pm.sock\nprograms:\n  - name: worker\n    command: /bin/echo\n    stdout_log: logs/worker.log\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ResolvePaths(&cfg, path)
	p := cfg.Programs[0]
	if !p.Autostart || p.Restart != "unexpected" || p.StopTimeout != "10s" {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if !cfg.Web.Enabled || cfg.Web.Listen != "127.0.0.1:19090" || cfg.EventHistory != 1000 {
		t.Fatalf("web defaults not applied: %+v", cfg)
	}
	if p.LogMaxBytes != 10*1024*1024 || p.LogBackups != 3 {
		t.Fatalf("log defaults not applied: %+v", p)
	}
	if p.Group != "default" {
		t.Fatalf("group default = %q", p.Group)
	}
	if cfg.Socket != filepath.Join(dir, "run/pm.sock") {
		t.Fatalf("socket = %q", cfg.Socket)
	}
	if p.StdoutLog != filepath.Join(dir, "logs/worker.log") {
		t.Fatalf("stdout log = %q", p.StdoutLog)
	}
}

func TestLoadOrDefaultAllowsMissingAndEmptyConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), DefaultFile)
	cfg, err := LoadOrDefault(missing)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "pm.sock" || cfg.StateDir != "." || !cfg.Web.Enabled || cfg.Web.Listen != "127.0.0.1:19090" || len(cfg.Programs) != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	empty, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty, cfg) {
		t.Fatalf("empty config = %+v, defaults = %+v", empty, cfg)
	}
}

func TestWebCanBeDisabled(t *testing.T) {
	cfg, err := Parse([]byte("web:\n  enabled: false\nprograms: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Enabled {
		t.Fatal("web should be disabled")
	}
}

func TestRejectsPublicWebWithoutToken(t *testing.T) {
	_, err := Parse([]byte("web:\n  enabled: true\n  listen: 0.0.0.0:19090\nprograms: []\n"))
	if err == nil {
		t.Fatal("expected public listener token error")
	}
}

func TestLoadRejectsDuplicateNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pm.yaml")
	data := []byte("programs:\n  - name: api\n    command: a\n  - name: api\n    command: b\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pm.yaml")
	data := []byte("programs:\n  - name: api\n    command: app\n    auto_start: true\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestPprofURLValidation(t *testing.T) {
	valid := []byte("programs:\n  - name: api\n    command: app\n    pprof_url: http://127.0.0.1:6060/debug/pprof\n")
	if _, err := Parse(valid); err != nil {
		t.Fatalf("valid pprof_url: %v", err)
	}
	invalid := []byte("programs:\n  - name: api\n    command: app\n    pprof_url: file:///tmp/profile\n")
	if _, err := Parse(invalid); err == nil {
		t.Fatal("expected invalid pprof_url error")
	}
}

func TestSeedDefaultConfigCreatesFileAndDoesNotOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "pm.yaml")

	if err := SeedDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	created, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected seeded file: %v", err)
	}
	if _, err := Parse(created); err != nil {
		t.Fatalf("seeded config must parse: %v", err)
	}

	// A user edit must be preserved on a subsequent call.
	userEdit := []byte("socket: custom.sock\n")
	if err := os.WriteFile(path, userEdit, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, userEdit) {
		t.Fatalf("seed overwrote existing file: %s", got)
	}
}

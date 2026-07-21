package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoolBanHub/pm/internal/config"
)

func TestUsageShowsShortDetachFlag(t *testing.T) {
	var output bytes.Buffer
	usage(&output)
	if !strings.Contains(output.String(), "daemon [-config FILE] [-d]") {
		t.Fatalf("usage does not advertise -d: %q", output.String())
	}
}

func TestLoadDaemonConfigUsesDefaultsOnlyWhenOptional(t *testing.T) {
	path := filepath.Join(t.TempDir(), config.DefaultFile)
	cfg, err := loadDaemonConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "pm.sock" || !cfg.Web.Enabled || len(cfg.Programs) != 0 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if _, err := loadDaemonConfig(path, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing config error = %v", err)
	}
}

func TestResolveControlSocketPriorityAndCurrentConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultFile)
	if err := os.WriteFile(path, []byte("socket: run/pm.sock\nweb:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fromConfig, err := resolveControlSocketAt("", "", path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "run/pm.sock"); fromConfig != want {
		t.Fatalf("socket from config = %q, want %q", fromConfig, want)
	}
	fromEnvironment, err := resolveControlSocketAt("", "/env/pm.sock", path)
	if err != nil || fromEnvironment != "/env/pm.sock" {
		t.Fatalf("socket from environment = %q, err = %v", fromEnvironment, err)
	}
	fromFlag, err := resolveControlSocketAt("/flag/pm.sock", "/env/pm.sock", path)
	if err != nil || fromFlag != "/flag/pm.sock" {
		t.Fatalf("socket from flag = %q, err = %v", fromFlag, err)
	}
	fallback, err := resolveControlSocketAt("", "", filepath.Join(dir, "missing.yaml"))
	if err != nil || fallback != config.DefaultSocketPath() {
		t.Fatalf("fallback socket = %q, err = %v", fallback, err)
	}
}

func TestFormatBytes(t *testing.T) {
	if got := formatBytes(2 * 1024 * 1024); got != "2.0MiB" {
		t.Fatalf("formatBytes = %q", got)
	}
}

func TestReadLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := readLastLines(file, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "three\nfour" {
		t.Fatalf("tail = %q", got)
	}
}

func TestReadLastLinesWithoutTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := readLastLines(file, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "three" {
		t.Fatalf("tail = %q", got)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote. Synchronous writers (fmt.Println/usage) fit the pipe buffer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	fn()
	writer.Close()
	os.Stdout = old
	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, reader); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	return buffer.String()
}

func TestRunVersionFlagPrintsInjectedVersion(t *testing.T) {
	saved := version
	version = "test-1.2.3"
	defer func() { version = saved }()
	output := captureStdout(t, func() {
		if err := run([]string{"-v"}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.TrimSpace(output) != "test-1.2.3" {
		t.Fatalf("-v output = %q", output)
	}
}

func TestRunHelpFlagsPrintUsage(t *testing.T) {
	for _, flag := range []string{"-h", "--help", "help"} {
		output := captureStdout(t, func() {
			if err := run([]string{flag}); err != nil {
				t.Fatalf("%s: %v", flag, err)
			}
		})
		if !strings.Contains(output, "Usage:") || !strings.Contains(output, "pm version | pm -v") {
			t.Fatalf("%s did not print usage: %q", flag, output)
		}
	}
}

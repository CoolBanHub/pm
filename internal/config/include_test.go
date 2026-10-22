package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIncludeMergesProgramsFromDropIns(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "pm.yaml")
	write(t, main, "programs:\n  - name: main-app\n    command: /bin/main\ninclude:\n  - conf.d/*.yaml\n")
	write(t, filepath.Join(dir, "conf.d", "a.yaml"), "programs:\n  - name: web\n    command: /bin/web\n    directory: webdir\n    stdout_log: web.log\n")
	write(t, filepath.Join(dir, "conf.d", "b.yaml"), "programs:\n  - name: db\n    command: /bin/db\n")

	cfg, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Programs) != 3 {
		t.Fatalf("programs = %d, want 3: %+v", len(cfg.Programs), cfg.Programs)
	}
	byName := map[string]Program{}
	for _, program := range cfg.Programs {
		byName[program.Name] = program
	}
	if byName["main-app"].Source != main {
		t.Fatalf("main program source = %q", byName["main-app"].Source)
	}
	if source := filepath.Join(dir, "conf.d", "a.yaml"); byName["web"].Source != source {
		t.Fatalf("web source = %q, want %q", byName["web"].Source, source)
	}

	// Include-defined paths anchor against the drop-in's directory so a conf.d
	// tree stays relocatable.
	ResolvePaths(&cfg, main)
	for _, program := range cfg.Programs {
		byName[program.Name] = program
	}
	if expected := filepath.Join(dir, "conf.d", "webdir"); byName["web"].Directory != expected {
		t.Fatalf("web directory = %q, want %q", byName["web"].Directory, expected)
	}
	if expected := filepath.Join(dir, "conf.d", "web.log"); byName["web"].StdoutLog != expected {
		t.Fatalf("web stdout log = %q, want %q", byName["web"].StdoutLog, expected)
	}
}

func TestIncludeNestedAndEmptyGlob(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "pm.yaml")
	write(t, main, "include:\n  - conf.d/*.yaml\n")
	write(t, filepath.Join(dir, "conf.d", "outer.yaml"), "include:\n  - inner/*.yaml\n")
	write(t, filepath.Join(dir, "conf.d", "inner", "deep.yaml"), "programs:\n  - name: deep\n    command: /bin/deep\n")
	write(t, main, "include:\n  - conf.d/*.yaml\n  - empty/*.yaml\n")

	cfg, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Programs) != 1 || cfg.Programs[0].Name != "deep" {
		t.Fatalf("nested include programs = %+v", cfg.Programs)
	}
}

func TestIncludeRejectsDuplicateAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "pm.yaml")
	write(t, main, "programs:\n  - name: api\n    command: /bin/main-api\ninclude:\n  - conf.d/*.yaml\n")
	write(t, filepath.Join(dir, "conf.d", "dup.yaml"), "programs:\n  - name: api\n    command: /bin/drop-in-api\n")

	_, err := Load(main)
	if err == nil || !strings.Contains(err.Error(), "duplicate program") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestIncludeRejectsDaemonSettingsInDropIn(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "pm.yaml")
	write(t, main, "include:\n  - conf.d/*.yaml\n")
	write(t, filepath.Join(dir, "conf.d", "bad.yaml"), "socket: other.sock\nprograms:\n  - name: api\n    command: /bin/api\n")

	_, err := Load(main)
	if err == nil || !strings.Contains(err.Error(), "drop-in files only support programs and include") {
		t.Fatalf("expected daemon-field rejection, got %v", err)
	}
}

func TestIncludeCycleDetected(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "pm.yaml")
	write(t, main, "include:\n  - conf.d/a.yaml\n")
	write(t, filepath.Join(dir, "conf.d", "a.yaml"), "include:\n  - ../pm.yaml\n")

	_, err := Load(main)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestParseRecordsIncludeWithoutExpanding(t *testing.T) {
	cfg, err := Parse([]byte("include:\n  - conf.d/*.yaml\nprograms:\n  - name: api\n    command: app\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Include) != 1 || cfg.Include[0] != "conf.d/*.yaml" {
		t.Fatalf("include = %+v", cfg.Include)
	}
	if cfg.Programs[0].Source != "" {
		t.Fatalf("anonymous parse must not stamp a source, got %q", cfg.Programs[0].Source)
	}
}

func TestProgramFieldDefaults(t *testing.T) {
	cfg, err := Parse([]byte("programs:\n  - name: api\n    command: app\n"))
	if err != nil {
		t.Fatal(err)
	}
	program := cfg.Programs[0]
	if program.Priority != DefaultPriority {
		t.Fatalf("priority default = %d", program.Priority)
	}
	if program.StartSecs != "0s" {
		t.Fatalf("start_secs default = %q", program.StartSecs)
	}
	if !program.ExpectedExit(0) || program.ExpectedExit(1) {
		t.Fatal("default exit codes should treat only 0 as expected")
	}
}

func TestExitCodesAndStartSecsValidation(t *testing.T) {
	if _, err := Parse([]byte("programs:\n  - name: api\n    command: app\n    start_secs: -3s\n")); err == nil {
		t.Fatal("expected negative start_secs rejection")
	}
	if _, err := Parse([]byte("programs:\n  - name: api\n    command: app\n    exit_codes: [0, 300]\n")); err == nil {
		t.Fatal("expected out-of-range exit code rejection")
	}
	cfg, err := Parse([]byte("programs:\n  - name: api\n    command: app\n    exit_codes: [0, 3]\n    start_secs: 5s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Programs[0].ExpectedExit(3) {
		t.Fatal("exit code 3 should be expected")
	}
}

func TestStopSignalAcceptsUserSignals(t *testing.T) {
	for _, signal := range []string{"USR1", "USR2"} {
		if _, err := Parse([]byte("programs:\n  - name: api\n    command: app\n    stop_signal: " + signal + "\n")); err != nil {
			t.Fatalf("signal %s rejected: %v", signal, err)
		}
	}
	if _, err := Parse([]byte("programs:\n  - name: api\n    command: app\n    stop_signal: KILL\n")); err == nil {
		t.Fatal("expected unsupported signal rejection")
	}
}

func TestEnvExpansionInProgramFields(t *testing.T) {
	t.Setenv("PM_TEST_BIN", "/expanded/bin")
	t.Setenv("PM_TEST_DIR", "/expanded/dir")
	cfg, err := Parse([]byte("programs:\n  - name: api\n    command: ${PM_TEST_BIN}/app\n    directory: ${PM_TEST_DIR}/work\n    environment:\n      GREETING: hello-${PM_TEST_BIN}\n    stdout_log: ${PM_TEST_DIR}/api.log\n"))
	if err != nil {
		t.Fatal(err)
	}
	program := cfg.Programs[0]
	if program.Command != "/expanded/bin/app" || program.Directory != "/expanded/dir/work" {
		t.Fatalf("expansion failed: %+v", program)
	}
	if program.Environment["GREETING"] != "hello-/expanded/bin" {
		t.Fatalf("environment expansion failed: %+v", program.Environment)
	}
	if program.StdoutLog != "/expanded/dir/api.log" {
		t.Fatalf("log expansion failed: %q", program.StdoutLog)
	}
}

func TestEnvExpansionLeavesPlainDollarTextAlone(t *testing.T) {
	cfg, err := Parse([]byte("programs:\n  - name: api\n    command: sh\n    args: [\"-c\", \"echo $HOME and ${UNDEFINED_PM_VAR} ok\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	argument := cfg.Programs[0].Args[1]
	// Bare $ passes through for the child shell to expand; an undefined ${VAR}
	// expands to the empty string (shell semantics).
	if !strings.Contains(argument, "$HOME") {
		t.Fatalf("bare $HOME must pass through: %q", argument)
	}
	if argument != "echo $HOME and  ok" {
		t.Fatalf("undefined ${VAR} must expand to empty: %q", argument)
	}
}

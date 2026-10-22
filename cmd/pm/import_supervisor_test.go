package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/CoolBanHub/pm/internal/config"
)

func TestReorderFlagsFirst(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"src", "-out", "dir"}, []string{"-out", "dir", "src"}},
		{[]string{"-out", "dir", "src"}, []string{"-out", "dir", "src"}},
		{[]string{"-force", "src"}, []string{"-force", "src"}},
		{[]string{"src"}, []string{"src"}},
		{[]string{"-out=dir", "src"}, []string{"-out=dir", "src"}},
		{[]string{"-", "src"}, []string{"-", "src"}},
	}
	for _, testCase := range cases {
		if got := reorderFlagsFirst(testCase.in); !reflect.DeepEqual(got, testCase.want) {
			t.Errorf("reorderFlagsFirst(%v) = %v, want %v", testCase.in, got, testCase.want)
		}
	}
}

// writeSupervisorFixture creates a small supervisor tree exercising the common
// fields PM converts.
func writeSupervisorFixture(t *testing.T, root string) {
	t.Helper()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "supervisord.conf"), `
[unix_http_server]
file=/var/run/supervisor.sock

[supervisord]
logfile=/var/log/supervisord.log

[include]
files = conf.d/*.conf

[group:web]
programs=nginx,api
`)
	write(filepath.Join(root, "conf.d", "apps.conf"), `
[program:nginx]
command=/usr/sbin/nginx -g "daemon off;"
directory=/etc/nginx
autostart=true
autorestart=true
startsecs=5
startretries=3
exitcodes=0,2
stopsignal=QUIT
stopwaitsecs=30
priority=10
stdout_logfile=/var/log/nginx/stdout.log
stdout_logfile_maxbytes=50MB
stdout_logfile_backups=5
redirect_stderr=true

[program:api]
command=/opt/api/bin/server --port %(ENV_API_PORT)s
environment=APP_ENV="prod",DEBUG='false'
autorestart=unexpected
priority=20
`)
}

func TestImportSupervisorConvertsConfig(t *testing.T) {
	// The imported drop-in carries ${API_PORT}; loading expands it from the
	// daemon environment, so pin it for the assertion.
	t.Setenv("API_PORT", "8080")
	source := filepath.Join(t.TempDir(), "supervisor")
	writeSupervisorFixture(t, source)
	out := filepath.Join(t.TempDir(), "out")

	if err := importSupervisorCommand([]string{source, "-out", out}); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(filepath.Join(out, config.DefaultFile))
	if err != nil {
		t.Fatalf("imported config must load: %v", err)
	}
	if len(cfg.Programs) != 2 {
		t.Fatalf("programs = %+v", cfg.Programs)
	}
	byName := map[string]config.Program{}
	for _, program := range cfg.Programs {
		byName[program.Name] = program
	}

	nginx := byName["nginx"]
	if nginx.Group != "web" || nginx.Priority != 10 || nginx.Restart != "always" {
		t.Fatalf("nginx mapping wrong: %+v", nginx)
	}
	if nginx.Command != "/usr/sbin/nginx" || len(nginx.Args) != 2 || nginx.Args[0] != "-g" || nginx.Args[1] != "daemon off;" {
		t.Fatalf("nginx command split wrong: %+v", nginx)
	}
	if nginx.StartSecs != "5s" || nginx.MaxRestarts != 3 || nginx.StopSignal != "QUIT" || nginx.StopTimeout != "30s" {
		t.Fatalf("nginx lifecycle mapping wrong: %+v", nginx)
	}
	if !reflect.DeepEqual(nginx.ExitCodes, []int{0, 2}) {
		t.Fatalf("nginx exit codes = %v", nginx.ExitCodes)
	}
	if nginx.StdoutLog != "/var/log/nginx/stdout.log" || nginx.StderrLog != nginx.StdoutLog {
		t.Fatalf("nginx log mapping wrong: %+v", nginx)
	}
	if nginx.LogMaxBytes != 50*1024*1024 || nginx.LogBackups != 5 {
		t.Fatalf("nginx rotation mapping wrong: %+v", nginx)
	}
	if !nginx.Autostart {
		t.Fatal("nginx must autostart")
	}

	api := byName["api"]
	if api.Group != "web" || api.Priority != 20 || api.Restart != "unexpected" {
		t.Fatalf("api mapping wrong: %+v", api)
	}
	if len(api.Args) != 2 || api.Args[1] != "8080" {
		t.Fatalf("api env ref conversion wrong: %+v", api.Args)
	}
	if api.Environment["APP_ENV"] != "prod" || api.Environment["DEBUG"] != "false" {
		t.Fatalf("api environment mapping wrong: %+v", api.Environment)
	}
	if api.StdoutLog != "logs/api.log" || api.StderrLog != "logs/api.error.log" {
		t.Fatalf("api default log paths wrong: %+v", api)
	}

	// Drop-in files must exist per program and be referenced from the main file.
	if len(cfg.Include) != 1 || cfg.Include[0] != "conf.d/*.yaml" {
		t.Fatalf("main include = %+v", cfg.Include)
	}
	for _, name := range []string{"nginx", "api"} {
		if _, err := os.Stat(filepath.Join(out, "conf.d", name+".yaml")); err != nil {
			t.Fatalf("drop-in for %s missing: %v", name, err)
		}
	}
}

func TestImportSupervisorRefusesOverwriteWithoutForce(t *testing.T) {
	source := filepath.Join(t.TempDir(), "supervisor")
	writeSupervisorFixture(t, source)
	out := filepath.Join(t.TempDir(), "out")

	if err := importSupervisorCommand([]string{source, "-out", out}); err != nil {
		t.Fatal(err)
	}
	err := importSupervisorCommand([]string{source, "-out", out})
	if err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("expected overwrite refusal, got %v", err)
	}
	if err := importSupervisorCommand([]string{source, "-out", out, "-force"}); err != nil {
		t.Fatalf("force re-import failed: %v", err)
	}
}

func TestCheckCommandReportsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "pm.yaml")
	if err := os.WriteFile(valid, []byte("programs:\n  - name: api\n    command: /bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkCommand([]string{"-config", valid}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	invalid := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(invalid, []byte("programs:\n  - name: api\n    restart: bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkCommand([]string{"-config", invalid}); err == nil {
		t.Fatal("broken config must fail check")
	}
}

func TestCheckCommandFollowsIncludes(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "pm.yaml")
	if err := os.WriteFile(main, []byte("include:\n  - conf.d/*.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf.d", "app.yaml"), []byte("programs:\n  - name: api\n    command: /bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkCommand([]string{"-config", main}); err != nil {
		t.Fatalf("include tree rejected: %v", err)
	}
}

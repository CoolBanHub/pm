package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/control"
)

const (
	systemdBinaryPath = "/usr/local/bin/pm"
	systemdUnitPath   = "/etc/systemd/system/pm.service"
)

type serviceIdentity struct {
	HomeDir string
	UID     int
	GID     int
}

func systemdCommand(args []string) error {
	flags := flag.NewFlagSet("systemd", flag.ContinueOnError)
	configPath := flags.String("config", "", "configuration file used by the system service")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("systemd does not accept positional arguments")
	}
	if runtime.GOOS != "linux" {
		return errors.New("systemd installation is only supported on Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("systemd installation requires root; run sudo pm systemd")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl was not found; this Linux system does not appear to use systemd")
	}

	identity, err := systemdIdentity()
	if err != nil {
		return err
	}
	if *configPath == "" {
		*configPath = defaultSystemdConfigPath(identity.HomeDir)
	}
	absoluteConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	if err := seedSystemdConfig(absoluteConfig, identity); err != nil {
		return err
	}
	cfg, err := config.Load(absoluteConfig)
	if err != nil {
		return err
	}
	config.ResolvePaths(&cfg, absoluteConfig)

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := installExecutable(executable, systemdBinaryPath); err != nil {
		return fmt.Errorf("install %s: %w", systemdBinaryPath, err)
	}
	unit, err := renderSystemdUnit(identity, absoluteConfig, cfg.Socket)
	if err != nil {
		return err
	}
	if err := writeInstalledFile(systemdUnitPath, strings.NewReader(unit), 0o644); err != nil {
		return fmt.Errorf("install %s: %w", systemdUnitPath, err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := shutdownExistingDaemon(cfg.Socket); err != nil {
		return err
	}
	if err := runSystemctl("enable", "pm.service"); err != nil {
		return err
	}
	if err := runSystemctl("restart", "pm.service"); err != nil {
		return err
	}
	if err := waitForSystemdDaemon(cfg.Socket, 5*time.Second); err != nil {
		return err
	}

	fmt.Printf("pm is managed by systemd and enabled at boot\n")
	fmt.Printf("service: pm.service\nconfig: %s\nsocket: %s\n", absoluteConfig, cfg.Socket)
	return nil
}

func systemdIdentity() (serviceIdentity, error) {
	username := os.Getenv("SUDO_USER")
	if username == "" {
		current, err := user.Current()
		if err != nil {
			return serviceIdentity{}, fmt.Errorf("resolve service user: %w", err)
		}
		username = current.Username
	}
	account, err := user.Lookup(username)
	if err != nil {
		return serviceIdentity{}, fmt.Errorf("resolve service user %q: %w", username, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return serviceIdentity{}, fmt.Errorf("invalid uid for %q: %w", username, err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return serviceIdentity{}, fmt.Errorf("invalid gid for %q: %w", username, err)
	}
	return serviceIdentity{HomeDir: account.HomeDir, UID: uid, GID: gid}, nil
}

func defaultSystemdConfigPath(home string) string {
	if workingDirectory, err := os.Getwd(); err == nil {
		candidate := filepath.Join(workingDirectory, config.DefaultFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(home, ".pm", config.DefaultFile)
}

func seedSystemdConfig(path string, identity serviceIdentity) error {
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return statErr
	}
	_, directoryStatErr := os.Stat(filepath.Dir(path))
	directoryCreated := errors.Is(directoryStatErr, os.ErrNotExist)
	if directoryStatErr != nil && !directoryCreated {
		return directoryStatErr
	}
	if err := config.SeedDefaultConfig(path); err != nil {
		return err
	}
	if !created {
		return nil
	}
	if directoryCreated {
		if err := os.Chown(filepath.Dir(path), identity.UID, identity.GID); err != nil {
			return fmt.Errorf("set config directory owner: %w", err)
		}
	}
	if err := os.Chown(path, identity.UID, identity.GID); err != nil {
		return fmt.Errorf("set config owner: %w", err)
	}
	return nil
}

func renderSystemdUnit(identity serviceIdentity, configPath, socket string) (string, error) {
	workingDirectory, err := systemdPath(filepath.Dir(configPath))
	if err != nil {
		return "", err
	}
	binary, err := systemdQuote(systemdBinaryPath)
	if err != nil {
		return "", err
	}
	configuration, err := systemdQuote(configPath)
	if err != nil {
		return "", err
	}
	controlSocket, err := systemdQuote(socket)
	if err != nil {
		return "", err
	}
	homeEnvironment, err := systemdQuote("HOME=" + identity.HomeDir)
	if err != nil {
		return "", err
	}
	socketEnvironment, err := systemdQuote("PM_SOCKET=" + socket)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`[Unit]
Description=PM Process Manager
After=network.target

[Service]
Type=simple
User=%d
Group=%d
WorkingDirectory=%s
Environment=%s
Environment=%s
ExecStart=%s daemon -config %s
ExecReload=%s -socket %s reload
Restart=on-failure
RestartSec=3
KillMode=control-group
KillSignal=SIGTERM
TimeoutStopSec=90
UMask=0027

[Install]
WantedBy=multi-user.target
`, identity.UID, identity.GID, workingDirectory, homeEnvironment, socketEnvironment, binary, configuration, binary, controlSocket), nil
}

func systemdPath(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("systemd path must be absolute: %s", value)
	}
	if strings.ContainsRune(value, '\x00') {
		return "", errors.New("systemd paths cannot contain NUL")
	}
	var escaped strings.Builder
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character == '%' {
			escaped.WriteString("%%")
			continue
		}
		if character == '/' || character == '.' || character == '_' || character == '-' ||
			(character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') {
			escaped.WriteByte(character)
			continue
		}
		fmt.Fprintf(&escaped, "\\x%02x", character)
	}
	return escaped.String(), nil
}

func systemdQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("systemd unit values cannot contain NUL or newlines")
	}
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`, nil
}

func installExecutable(source, destination string) error {
	resolvedSource, sourceErr := filepath.EvalSymlinks(source)
	resolvedDestination, destinationErr := filepath.EvalSymlinks(destination)
	if sourceErr == nil && destinationErr == nil && resolvedSource == resolvedDestination {
		return nil
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	return writeInstalledFile(destination, file, 0o755)
}

func writeInstalledFile(path string, source io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pm-install-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, source); err != nil {
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
	return os.Rename(temporaryPath, path)
}

func runSystemctl(args ...string) error {
	command := exec.Command("systemctl", args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func shutdownExistingDaemon(socket string) error {
	response, err := control.Call(socket, control.Request{Action: "shutdown"})
	if err != nil {
		return nil
	}
	if !response.OK {
		return errors.New(response.Message)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := control.Call(socket, control.Request{Action: "status"}); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("existing daemon did not stop before systemd handoff")
}

func waitForSystemdDaemon(socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := control.Call(socket, control.Request{Action: "status"})
		if err == nil && response.OK {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("systemd started pm.service, but the daemon did not become ready; inspect journalctl -u pm.service")
}

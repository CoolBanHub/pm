package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/control"
	"github.com/CoolBanHub/pm/internal/supervisor"
	webui "github.com/CoolBanHub/pm/internal/web"
)

var version = "dev"

// llmsText is the AI-readable usage/configuration guide printed by
// `pm llms.txt`. It works offline and needs no running daemon.
//
//go:embed llms.txt
var llmsText string

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pm:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("pm", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	socket := global.String("socket", "", "Unix socket path")
	showVersion := global.Bool("v", false, "print version and exit")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	args = global.Args()
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("a command is required")
	}

	command := args[0]
	args = args[1:]
	switch command {
	case "daemon":
		return daemonCommand(args)
	case "status", "start", "stop", "restart":
		resolvedSocket, err := resolveControlSocket(*socket)
		if err != nil {
			return err
		}
		return controlCommand(resolvedSocket, command, args)
	case "reload", "shutdown":
		if len(args) != 0 {
			return fmt.Errorf("%s does not accept arguments", command)
		}
		resolvedSocket, err := resolveControlSocket(*socket)
		if err != nil {
			return err
		}
		return controlCommand(resolvedSocket, command, nil)
	case "list":
		resolvedSocket, err := resolveControlSocket(*socket)
		if err != nil {
			return err
		}
		return listCommand(resolvedSocket, args)
	case "logs":
		resolvedSocket, err := resolveControlSocket(*socket)
		if err != nil {
			return err
		}
		return logsCommand(resolvedSocket, args)
	case "version":
		fmt.Println(version)
		return nil
	case "llms.txt", "llms":
		fmt.Print(llmsText)
		return nil
	case "help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", command)
	}
}

func daemonCommand(args []string) error {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	configPath := flags.String("config", defaultConfigPath(), "configuration file")
	detach := flags.Bool("d", false, "run in the background")
	legacyDetach := flags.Bool("detach", false, "deprecated alias for -d")
	child := flags.Bool("child", false, "internal detached child marker")
	optionalConfig := flags.Bool("config-optional", false, "internal optional config marker")
	daemonLog := flags.String("log", "", "detached daemon log")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("daemon does not accept positional arguments")
	}
	absoluteConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	configExplicit := flagWasSet(flags, "config") && !*optionalConfig
	if !configExplicit {
		// First run: materialize ~/.pm/pm.yaml so the default configuration is
		// visible and editable. Failure is non-fatal; we fall back to built-in
		// defaults via LoadOrDefault below.
		if err := config.SeedDefaultConfig(absoluteConfig); err != nil {
			log.Printf("seed default config: %v", err)
		}
	}
	cfg, err := loadDaemonConfig(absoluteConfig, !configExplicit)
	if err != nil {
		return err
	}
	config.ResolvePaths(&cfg, absoluteConfig)
	if (*detach || *legacyDetach) && !*child {
		return detachDaemon(absoluteConfig, cfg, *daemonLog, !configExplicit)
	}
	return serveDaemon(absoluteConfig, cfg)
}

// defaultConfigPath picks the configuration file used when -config is omitted:
// a pm.yaml in the current directory wins (backward-compatible project-local
// usage), otherwise the home default ~/.pm/pm.yaml is used.
func defaultConfigPath() string {
	if workingDirectory, err := os.Getwd(); err == nil {
		candidate := filepath.Join(workingDirectory, config.DefaultFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return config.DefaultConfigPath()
}

func loadDaemonConfig(path string, optional bool) (config.Config, error) {
	if optional {
		return config.LoadOrDefault(path)
	}
	return config.Load(path)
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	found := false
	flags.Visit(func(item *flag.Flag) {
		if item.Name == name {
			found = true
		}
	})
	return found
}

func resolveControlSocket(explicit string) (string, error) {
	return resolveControlSocketAt(explicit, os.Getenv("PM_SOCKET"), defaultConfigPath())
}

func resolveControlSocketAt(explicit, environment, configPath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if environment != "" {
		return environment, nil
	}
	cfg, err := config.Load(configPath)
	if err == nil {
		config.ResolvePaths(&cfg, configPath)
		return cfg.Socket, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return config.DefaultSocketPath(), nil
}

func serveDaemon(configPath string, cfg config.Config) error {
	logger := log.New(os.Stderr, "pm: ", log.LstdFlags)
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events, err := supervisor.NewEventStore(filepath.Join(cfg.StateDir, "events.jsonl"), cfg.EventHistory)
	if err != nil {
		return err
	}
	defer events.Close()
	manager := supervisor.NewWithEvents(cfg.Programs, events)
	for _, err := range manager.Autostart() {
		logger.Printf("autostart: %v", err)
	}
	server := control.NewServer(cfg, configPath, manager, cancel, logger)
	type serviceResult struct {
		name string
		err  error
	}
	results := make(chan serviceResult, 2)
	serviceCount := 1
	go func() { results <- serviceResult{name: "control", err: server.Serve(ctx)} }()
	if cfg.Web.Enabled {
		token, tokenErr := cfg.Web.ResolvedToken()
		if tokenErr != nil {
			cancel()
			<-results
			return tokenErr
		}
		webServer := webui.NewServer(cfg.Web.Listen, token, server, logger)
		serviceCount++
		go func() { results <- serviceResult{name: "web", err: webServer.Serve(ctx)} }()
	}
	logger.Printf("control socket listening on %s", cfg.Socket)

	var serviceErrs []error
	select {
	case result := <-results:
		serviceCount--
		if result.err != nil {
			serviceErrs = append(serviceErrs, fmt.Errorf("%s service: %w", result.name, result.err))
		}
		cancel()
	case <-ctx.Done():
		cancel()
	}
	for serviceCount > 0 {
		result := <-results
		serviceCount--
		if result.err != nil {
			serviceErrs = append(serviceErrs, fmt.Errorf("%s service: %w", result.name, result.err))
		}
	}
	logger.Printf("stopping managed programs")
	stopErr := server.Manager().StopAll()
	serviceErrs = append(serviceErrs, stopErr)
	return errors.Join(serviceErrs...)
}

func detachDaemon(configPath string, cfg config.Config, logPath string, optionalConfig bool) error {
	if connection, err := net.DialTimeout("unix", cfg.Socket, 200*time.Millisecond); err == nil {
		connection.Close()
		return fmt.Errorf("daemon is already listening on %s", cfg.Socket)
	}
	if logPath == "" {
		logPath = filepath.Join(filepath.Dir(configPath), "logs", "pm-daemon.log")
	}
	if !filepath.IsAbs(logPath) {
		logPath = filepath.Join(filepath.Dir(configPath), logPath)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	childArgs := []string{"daemon", "-config", configPath, "-child"}
	if optionalConfig {
		childArgs = append(childArgs, "-config-optional")
	}
	cmd := exec.Command(executable, childArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := control.Call(cfg.Socket, control.Request{Action: "status"})
		if err == nil && response.OK && webReady(cfg.Web) {
			fmt.Printf("daemon started (pid %d, log %s)\n", pid, logPath)
			if cfg.Web.Enabled {
				fmt.Printf("web administration: http://%s\n", cfg.Web.Listen)
			}
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready; check %s", logPath)
}

func webReady(cfg config.Web) bool {
	if !cfg.Enabled {
		return true
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/api/v1/session", nil)
	if err != nil {
		return false
	}
	if token, err := cfg.ResolvedToken(); err == nil && token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 300 * time.Millisecond}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func controlCommand(socket, action string, names []string) error {
	if len(names) == 0 && action != "status" && action != "reload" && action != "shutdown" {
		return fmt.Errorf("%s requires a program name or all", action)
	}
	response, err := control.Call(socket, control.Request{Action: action, Names: names})
	if err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Message)
	}
	if action == "status" {
		printStatuses(response.Processes)
	} else if response.Message != "" {
		fmt.Println(response.Message)
	}
	return nil
}

func listCommand(socket string, names []string) error {
	response, err := control.Call(socket, control.Request{Action: "status", Names: names})
	if err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Message)
	}
	printList(response.Processes)
	return nil
}

func printStatuses(statuses []supervisor.Status) {
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tGROUP\tSTATE\tPID\tCPU\tMEMORY\tCHILDREN\tDESCENDANTS\tGOROUTINES\tUPTIME\tSTARTS\tEXIT\tDETAIL")
	for _, status := range statuses {
		pid, cpu, memory, children, descendants, goroutines, exit := "-", "-", "-", "-", "-", "-", "-"
		if status.PID != 0 {
			pid = strconv.Itoa(status.PID)
			cpu = fmt.Sprintf("%.1f%%", status.CPU)
			memory = formatBytes(status.Memory)
			children = strconv.Itoa(status.Children)
			descendants = strconv.Itoa(status.Descendants)
			if status.Goroutines != nil {
				goroutines = strconv.Itoa(*status.Goroutines)
			}
		}
		if status.ExitCode != nil {
			exit = strconv.Itoa(*status.ExitCode)
		}
		detail := status.LastError
		if status.Restarts > 0 {
			if detail != "" {
				detail += "; "
			}
			detail += fmt.Sprintf("restarts=%d", status.Restarts)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", status.Name, status.Group, status.State, pid, cpu, memory, children, descendants, goroutines, valueOrDash(status.Uptime), status.Starts, exit, detail)
	}
	_ = writer.Flush()
}

// printList renders a compact overview of managed processes. Unlike
// printStatuses it omits CPU, memory, restart and exit detail for a quick scan.
func printList(statuses []supervisor.Status) {
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tGROUP\tSTATE\tPID\tUPTIME")
	for _, status := range statuses {
		pid := "-"
		if status.PID != 0 {
			pid = strconv.Itoa(status.PID)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", status.Name, status.Group, status.State, pid, valueOrDash(status.Uptime))
	}
	_ = writer.Flush()
}

func formatBytes(value int64) string {
	const (
		kib = int64(1024)
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case value >= gib:
		return fmt.Sprintf("%.1fGiB", float64(value)/float64(gib))
	case value >= mib:
		return fmt.Sprintf("%.1fMiB", float64(value)/float64(mib))
	case value >= kib:
		return fmt.Sprintf("%.1fKiB", float64(value)/float64(kib))
	default:
		return fmt.Sprintf("%dB", value)
	}
}

func logsCommand(socket string, args []string) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	lines := flags.Int("n", 100, "number of lines")
	follow := flags.Bool("f", false, "follow log output")
	useStderr := flags.Bool("stderr", false, "show stderr log")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("logs requires exactly one program name")
	}
	if *lines < 0 {
		return errors.New("line count cannot be negative")
	}
	response, err := control.Call(socket, control.Request{Action: "status", Names: []string{flags.Arg(0)}})
	if err != nil {
		return err
	}
	if !response.OK {
		return errors.New(response.Message)
	}
	path := response.Processes[0].StdoutLog
	if *useStderr {
		path = response.Processes[0].StderrLog
	}
	if path == "" {
		return errors.New("the selected log file is not configured")
	}
	return showLog(path, *lines, *follow)
}

func showLog(path string, lines int, follow bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	content, err := readLastLines(file, lines)
	if err != nil {
		return err
	}
	if len(content) > 0 {
		fmt.Println(string(content))
	}
	if !follow {
		return nil
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			fmt.Print(line)
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			return readErr
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func readLastLines(file *os.File, lines int) ([]byte, error) {
	if lines == 0 {
		return nil, nil
	}
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
		if _, err := file.ReadAt(chunk, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		data = append(chunk, data...)
	}
	data = bytes.TrimSuffix(data, []byte{'\n'})
	parts := bytes.Split(data, []byte{'\n'})
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return bytes.Join(parts, []byte{'\n'}), nil
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func usage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage:
	pm [-socket PATH] daemon [-config FILE] [-d] [-log FILE]
  pm [-socket PATH] status [NAME...]
  pm [-socket PATH] list [NAME...]
  pm [-socket PATH] start|stop|restart NAME|all
  pm [-socket PATH] reload|shutdown
  pm [-socket PATH] logs [-n LINES] [-f] [-stderr] NAME
  pm llms.txt
  pm version | pm -v
  pm help | pm -h | pm --help`)
}

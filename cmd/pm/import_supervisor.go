package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/CoolBanHub/pm/internal/config"

	"gopkg.in/yaml.v3"
)

// reorderFlagsFirst moves flag arguments (together with their values) before
// positional arguments so both "pm import-supervisor SRC -out DIR" and
// "pm import-supervisor -out DIR SRC" parse. A lone "-" is positional; "--"
// terminates the flag section.
func reorderFlagsFirst(args []string) []string {
	var flagArgs, positional []string
	terminated := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if terminated || arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		if arg == "--" {
			terminated = true
			continue
		}
		flagArgs = append(flagArgs, arg)
		if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return append(flagArgs, positional...)
}

// importSupervisorCommand converts a supervisor (supervisord) configuration
// tree into PM configuration files: one drop-in YAML per program under
// <out>/conf.d/, plus a main <out>/pm.yaml that includes them. The output is
// validated with PM's own loader before the command reports success.
//
// SRC may be a supervisord.conf file or a directory containing supervisor
// .conf/.ini files (e.g. /etc/supervisor). [program:x] and [group:x]
// sections are converted; unsupported sections and keys produce warnings
// instead of failures.
func importSupervisorCommand(args []string) error {
	flags := flag.NewFlagSet("import-supervisor", flag.ContinueOnError)
	outDir := flags.String("out", "", "output directory (default ~/.pm)")
	force := flags.Bool("force", false, "overwrite existing output files")
	if err := flags.Parse(reorderFlagsFirst(args)); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("import-supervisor requires exactly one source path (supervisord.conf file or directory)")
	}
	source := flags.Arg(0)
	if *outDir == "" {
		*outDir = config.HomeDir()
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect source: %w", err)
	}

	var files []string
	if sourceInfo.IsDir() {
		files, err = supervisorFilesInDir(source)
	} else {
		files = []string{source}
	}
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no supervisor .conf/.ini files found under %s", source)
	}

	sections, warnings := parseSupervisorFiles(files)
	programs, groups, sectionWarnings := convertSupervisorSections(sections)
	warnings = append(warnings, sectionWarnings...)
	if len(programs) == 0 {
		if len(warnings) > 0 {
			for _, warning := range warnings {
				fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
			}
		}
		return errors.New("no [program:x] sections found; nothing to import")
	}
	for name, group := range groups {
		if program, ok := programs[name]; ok {
			program.Group = group
			programs[name] = program
		}
	}

	if err := writeImportOutput(*outDir, programs, *force); err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	names := make([]string, 0, len(programs))
	for name := range programs {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Printf("imported %d program(s) into %s: %s\n", len(programs), *outDir, strings.Join(names, ", "))
	fmt.Printf("next: pm check -config %s && pm daemon -config %s\n", filepath.Join(*outDir, config.DefaultFile), filepath.Join(*outDir, config.DefaultFile))
	return nil
}

// supervisorFilesInDir collects candidate supervisor configuration files from
// a directory tree: *.conf and *.ini, supervisor's two conventional extensions.
func supervisorFilesInDir(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension == ".conf" || extension == ".ini" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// supervisorSection is one parsed [header] with its ordered key/value pairs.
type supervisorSection struct {
	header string
	file   string
	values map[string]string
}

func parseSupervisorFiles(files []string) ([]supervisorSection, []string) {
	var sections []supervisorSection
	var warnings []string
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skip %s: %v", file, err))
			continue
		}
		fileSections, fileWarnings := parseSupervisorINI(file, string(content))
		sections = append(sections, fileSections...)
		warnings = append(warnings, fileWarnings...)
	}
	return sections, warnings
}

// parseSupervisorINI parses the ConfigParser-style ini format supervisor
// uses: [section] headers, key=value pairs, ;/# comments. Line continuations
// and interpolation are left to the converters.
func parseSupervisorINI(file, content string) ([]supervisorSection, []string) {
	var sections []supervisorSection
	var warnings []string
	var current *supervisorSection
	for lineNumber, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sections = append(sections, supervisorSection{header: strings.TrimSpace(line[1 : len(line)-1]), file: file, values: map[string]string{}})
			current = &sections[len(sections)-1]
			continue
		}
		if current == nil {
			warnings = append(warnings, fmt.Sprintf("%s:%d: key %q outside any section ignored", file, lineNumber+1, line))
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			warnings = append(warnings, fmt.Sprintf("%s:%d: malformed line %q ignored", file, lineNumber+1, line))
			continue
		}
		current.values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return sections, warnings
}

// convertSupervisorSections maps [program:x]/[group:x] sections to PM
// programs and group assignments; everything else is reported as skipped.
func convertSupervisorSections(sections []supervisorSection) (map[string]config.Program, map[string]string, []string) {
	programs := map[string]config.Program{}
	groups := map[string]string{}
	var warnings []string
	for _, section := range sections {
		switch {
		case strings.HasPrefix(section.header, "program:"):
			name := section.header[len("program:"):]
			program, programWarnings := convertSupervisorProgram(section, name)
			warnings = append(warnings, programWarnings...)
			if _, exists := programs[name]; exists {
				warnings = append(warnings, fmt.Sprintf("%s: duplicate program %q keeps the first definition", section.file, name))
				continue
			}
			programs[name] = program
		case strings.HasPrefix(section.header, "group:"):
			name := section.header[len("group:"):]
			for _, member := range splitSupervisorList(section.values["programs"]) {
				groups[member] = name
			}
		case strings.HasPrefix(section.header, "eventlistener:"), strings.HasPrefix(section.header, "fcgi-program:"):
			warnings = append(warnings, fmt.Sprintf("%s: [%s] is not supported by PM and was skipped", section.file, section.header))
		case section.header == "supervisord", section.header == "unix_http_server", section.header == "rpcinterface:supervisor", section.header == "supervisorctl", section.header == "include":
			// Daemon-level supervisor settings have no PM equivalent; the
			// imported main config carries PM's own defaults.
		default:
			warnings = append(warnings, fmt.Sprintf("%s: unknown section [%s] skipped", section.file, section.header))
		}
	}
	return programs, groups, warnings
}

// convertSupervisorProgram maps one [program:x] section to a PM program.
func convertSupervisorProgram(section supervisorSection, name string) (config.Program, []string) {
	var warnings []string
	values := section.values
	program := config.DefaultProgram()
	program.Name = name
	program.Group = "default"
	program.Source = ""

	if numprocs, _ := strconv.Atoi(values["numprocs"]); numprocs > 1 {
		warnings = append(warnings, fmt.Sprintf("%s: program %q numprocs=%d is not supported; only a single instance named %q was imported", section.file, name, numprocs, name))
	}
	command, args, err := splitSupervisorCommand(values["command"])
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("%s: program %q command: %v", section.file, name, err))
	}
	program.Command = expandSupervisorRef(command, name)
	for i, arg := range args {
		args[i] = expandSupervisorRef(arg, name)
	}
	program.Args = args
	program.Directory = expandSupervisorRef(values["directory"], name)
	if autostart, found := values["autostart"]; found {
		program.Autostart = !strings.EqualFold(autostart, "false")
	}
	switch strings.ToLower(values["autorestart"]) {
	case "true":
		program.Restart = "always"
	case "false":
		program.Restart = "never"
	default: // "unexpected" or unset
		program.Restart = "unexpected"
	}
	if startsecs, found := values["startsecs"]; found {
		if seconds, err := strconv.Atoi(startsecs); err == nil && seconds > 0 {
			program.StartSecs = fmt.Sprintf("%ds", seconds)
		}
	}
	if retries, found := values["startretries"]; found {
		if parsed, err := strconv.Atoi(retries); err == nil && parsed >= 0 {
			program.MaxRestarts = parsed
		}
	}
	if codes, found := values["exitcodes"]; found && strings.TrimSpace(codes) != "" {
		for _, raw := range splitSupervisorList(codes) {
			if code, err := strconv.Atoi(raw); err == nil {
				program.ExitCodes = append(program.ExitCodes, code)
			}
		}
	}
	if priority, found := values["priority"]; found {
		if parsed, err := strconv.Atoi(priority); err == nil {
			program.Priority = parsed
		}
	}
	if signal := strings.ToUpper(values["stopsignal"]); signal != "" {
		program.StopSignal = signal
	}
	if wait, found := values["stopwaitsecs"]; found {
		if seconds, err := strconv.Atoi(wait); err == nil && seconds > 0 {
			program.StopTimeout = fmt.Sprintf("%ds", seconds)
		}
	}
	program.StdoutLog = importLogPath(values["stdout_logfile"], name, "")
	if redirect, found := values["redirect_stderr"]; found && strings.EqualFold(redirect, "true") {
		program.StderrLog = program.StdoutLog
	} else {
		program.StderrLog = importLogPath(values["stderr_logfile"], name, ".error")
	}
	if maxbytes, found := values["stdout_logfile_maxbytes"]; found {
		if parsed, err := parseSupervisorBytes(maxbytes); err == nil {
			program.LogMaxBytes = parsed
		} else {
			warnings = append(warnings, fmt.Sprintf("%s: program %q stdout_logfile_maxbytes %q kept PM default", section.file, name, maxbytes))
		}
	}
	if backups, found := values["stdout_logfile_backups"]; found {
		if parsed, err := strconv.Atoi(backups); err == nil && parsed >= 0 {
			program.LogBackups = parsed
		}
	}
	if environment, found := values["environment"]; found && strings.TrimSpace(environment) != "" {
		parsed, parseWarnings := parseSupervisorEnvironment(section.file, name, environment)
		warnings = append(warnings, parseWarnings...)
		program.Environment = parsed
	}
	if user := values["user"]; user != "" {
		warnings = append(warnings, fmt.Sprintf("%s: program %q user=%q ignored: PM runs programs as the invoking user (use systemd User= to drop privileges)", section.file, name, user))
	}
	if umask := values["umask"]; umask != "" {
		warnings = append(warnings, fmt.Sprintf("%s: program %q umask=%q is not supported and was skipped", section.file, name, umask))
	}
	return program, warnings
}

// importLogPath maps a supervisor logfile setting to a PM log path. Unset or
// "AUTO" becomes the PM convention logs/<name>.log; "NONE" keeps PM's default
// of inheriting daemon output only when both streams are NONE, expressed as
// an empty path.
func importLogPath(value, name, suffix string) string {
	value = strings.TrimSpace(value)
	switch strings.ToUpper(value) {
	case "", "AUTO":
		return fmt.Sprintf("logs/%s%s.log", name, suffix)
	case "NONE":
		return ""
	default:
		return expandSupervisorRef(value, name)
	}
}

// parseSupervisorBytes parses supervisor byte sizes: plain integers or with a
// KB/MB/GB/TB suffix (case-insensitive).
func parseSupervisorBytes(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	multiplier := int64(1)
	if len(value) > 2 {
		for _, unit := range []struct {
			suffix string
			factor int64
		}{
			{"tb", 1 << 40}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10},
		} {
			if strings.HasSuffix(value, unit.suffix) {
				multiplier = unit.factor
				value = strings.TrimSpace(strings.TrimSuffix(value, unit.suffix))
				break
			}
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return parsed * multiplier, nil
}

// parseSupervisorEnvironment parses supervisor's environment=KEY="v",K2='v2'
// syntax into a map.
func parseSupervisorEnvironment(file, name, value string) (map[string]string, []string) {
	result := map[string]string{}
	var warnings []string
	for _, item := range splitSupervisorList(value) {
		key, itemValue, found := strings.Cut(item, "=")
		if !found {
			warnings = append(warnings, fmt.Sprintf("%s: program %q environment entry %q ignored", file, name, item))
			continue
		}
		itemValue = strings.Trim(itemValue, `"'`)
		result[strings.TrimSpace(key)] = expandSupervisorRef(itemValue, name)
	}
	return result, warnings
}

// splitSupervisorList splits a comma-separated supervisor value.
func splitSupervisorList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// splitSupervisorCommand splits supervisor's command= value the way
// supervisord does (shlex-style with quote support) into command plus args.
func splitSupervisorCommand(value string) (string, []string, error) {
	fields, err := splitQuoted(value)
	if err != nil {
		return "", nil, fmt.Errorf("%w", err)
	}
	if len(fields) == 0 || fields[0] == "" {
		return "", nil, errors.New("command is empty")
	}
	return fields[0], fields[1:], nil
}

// splitQuoted performs shell-style word splitting honoring single and double
// quotes.
func splitQuoted(value string) ([]string, error) {
	var fields []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() > 0 {
			fields = append(fields, current.String())
			current.Reset()
		}
	}
	for _, char := range value {
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
				continue
			}
			current.WriteRune(char)
		case char == '\'' || char == '"':
			quote = char
		case char == ' ' || char == '\t':
			flush()
		default:
			current.WriteRune(char)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quote in %q", value)
	}
	flush()
	return fields, nil
}

// supervisorRef matches supervisor's %(NAME)s interpolation references.
var supervisorRef = regexp.MustCompile(`%\((ENV_)?([A-Za-z0-9_]+)\)s`)

// expandSupervisorRef rewrites supervisor's %(ENV_NAME)s interpolation into
// PM's ${NAME} form (keeping expansion live after import) and resolves
// %(program_name)s to the program's own name at import time.
func expandSupervisorRef(value, programName string) string {
	if !strings.Contains(value, "%(") {
		return value
	}
	return supervisorRef.ReplaceAllStringFunc(value, func(match string) string {
		groups := supervisorRef.FindStringSubmatch(match)
		if groups[1] != "" {
			return "${" + groups[2] + "}"
		}
		if groups[2] == "program_name" {
			return programName
		}
		return match
	})
}

// writeImportOutput writes the main config plus one drop-in file per program
// and validates the result by loading it with PM's loader.
func writeImportOutput(outDir string, programs map[string]config.Program, force bool) error {
	mainPath := filepath.Join(outDir, config.DefaultFile)
	confDir := filepath.Join(outDir, "conf.d")
	if !force {
		if _, err := os.Stat(mainPath); err == nil {
			return fmt.Errorf("%s already exists; use -force to overwrite", mainPath)
		}
	}
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}

	cfg := config.DefaultConfig()
	cfg.Include = []string{"conf.d/*.yaml"}
	cfg.Programs = nil
	main, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(mainPath, append(main, '\n'), 0o600); err != nil {
		return err
	}

	names := make([]string, 0, len(programs))
	for name := range programs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		program := programs[name]
		program.Source = ""
		dropIn := struct {
			Programs []config.Program `yaml:"programs"`
		}{Programs: []config.Program{program}}
		content, err := yaml.Marshal(&dropIn)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(confDir, name+".yaml"), append(content, '\n'), 0o600); err != nil {
			return err
		}
	}

	// Validate the imported tree with the real loader so a broken import can
	// never be reported as success.
	loaded, err := config.Load(mainPath)
	if err != nil {
		return fmt.Errorf("imported configuration failed validation: %w", err)
	}
	if len(loaded.Programs) != len(programs) {
		return fmt.Errorf("imported configuration defines %d programs, expected %d", len(loaded.Programs), len(programs))
	}
	return nil
}

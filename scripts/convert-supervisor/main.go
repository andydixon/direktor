// scripts/convert-supervisor turns a supervisor config tree into a
// native Direktor layout. It exists for people who want to leave the
// `[supervisord]` / `[program:foo]` shape behind once they've migrated.
//
// You don't *have* to use this. Direktor will happily run on a verbatim
// supervisord.conf forever — that's the whole point of the parser. This
// tool is for the cleanup pass after the migration's settled. It writes:
//
//	OUTPUT_DIR/direktor.conf       — globals + [include] glob
//	OUTPUT_DIR/conf.d/<name>.conf  — one per program
//	OUTPUT_DIR/conf.d/group-<n>.conf — one per group
//
// While it walks the source files it also flags anything supervisor allowed
// that we don't (event listeners, third-party plugin sections, unknown
// keys). Those get printed as warnings on stderr — the tool doesn't fail
// on them, because in the real world there's almost always *something*
// weird in a long-lived supervisor config and we'd rather you saw the warnings
// and made an informed decision than have us refuse to convert.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/andydixon/direktor/internal/config"
	"github.com/andydixon/direktor/pkg/types"
)

var (
	// errUsage is the sentinel for "your flags are wrong" — we use it so the
	// caller can print the usage banner without having to string-match.
	errUsage = errors.New("invalid usage")

	// The "we know about these keys" sets. Anything outside these gets a
	// warning. Maps-as-sets because Go doesn't have proper sets and using
	// `map[string]struct{}{}` is the idiomatic dance.
	supervisorKeys = map[string]struct{}{
		"logfile":         {},
		"loglevel":        {},
		"pidfile":         {},
		"nodaemon":        {},
		"minfds":          {},
		"minprocs":        {},
		"identifier":      {},
		"socket_path":     {},
		"socket_owner":    {},
		"socket_mode":     {},
		"http_port":       {},
		"http_host":       {},
		"http_auth":       {},
		"http_auth_token": {},
	}
	emailKeys = map[string]struct{}{
		"enabled":    {},
		"smtp_host":  {},
		"smtp_port":  {},
		"username":   {},
		"password":   {},
		"from":       {},
		"recipients": {},
		"use_tls":    {},
		"notify_on":  {},
	}
	programKeys = map[string]struct{}{
		"command":                 {},
		"directory":               {},
		"user":                    {},
		"autostart":               {},
		"autorestart":             {},
		"startsecs":               {},
		"startretries":            {},
		"stopsignal":              {},
		"stopwaitsecs":            {},
		"exitcodes":               {},
		"priority":                {},
		"numprocs":                {},
		"numprocs_start":          {},
		"stdout_logfile":          {},
		"stderr_logfile":          {},
		"stdout_logfile_maxbytes": {},
		"stderr_logfile_maxbytes": {},
		"stdout_logfile_backups":  {},
		"stderr_logfile_backups":  {},
		"redirect_stderr":         {},
		"environment":             {},
	}
	groupKeys = map[string]struct{}{
		"programs": {},
		"priority": {},
	}
)

// iniFile keeps the parsed sections AND the order they appeared in. We need
// the order for reproducible output — otherwise `convert-supervisor` would
// emit different (but equivalent) configs run-to-run, which makes diffs
// unreviewable.
type iniFile struct {
	Sections map[string]map[string]string
	Order    []string
}

// warning is a "we saw something we don't support" finding. Path lets the
// user jump straight to the offending file.
type warning struct {
	Path    string
	Section string
	Key     string
	Reason  string
}

func main() {
	var inputPath string
	var outputDir string

	flag.StringVar(&inputPath, "input", "", "Path to the Supervisor config entrypoint")
	flag.StringVar(&outputDir, "output-dir", "", "Directory to write Direktor config files into")
	flag.Parse()

	if err := run(inputPath, outputDir); err != nil {
		if errors.Is(err, errUsage) {
			printUsage()
		}
		fmt.Fprintf(os.Stderr, "convert error: %v\n", err)
		os.Exit(1)
	}
}

// run is the actual orchestration. main() is just a thin "parse flags and
// surface errors" wrapper around this so the pieces are testable in
// isolation if we ever fancy it.
func run(inputPath, outputDir string) error {
	if strings.TrimSpace(inputPath) == "" || strings.TrimSpace(outputDir) == "" {
		return errUsage
	}

	inputPath, err := filepath.Abs(inputPath)
	if err != nil {
		return fmt.Errorf("resolve input path: %w", err)
	}

	outputDir, err = filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}

	// Two parses of the same tree, deliberately:
	//   - config.Parse gives us the structured types.Config we render from.
	//   - parseINIWithWarnings gives us the raw section names + keys so we
	//     can spot anything we don't recognise and warn about it.
	// The supervisor-compat parser doesn't expose unknown sections (it
	// silently drops them), which is the right behaviour for the runtime
	// daemon but no good for a converter.
	cfg, err := config.Parse(inputPath)
	if err != nil {
		return fmt.Errorf("parse Supervisor config: %w", err)
	}

	raw, warnings, err := parseINIWithWarnings(inputPath)
	if err != nil {
		return fmt.Errorf("inspect Supervisor config: %w", err)
	}

	if err := writeOutput(outputDir, raw, cfg); err != nil {
		return err
	}

	printWarnings(warnings)
	fmt.Fprintf(os.Stderr, "Wrote Direktor config to %s\n", outputDir)
	return nil
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  go run ./scripts/convert-supervisor --input /etc/supervisor/supervisord.conf --output-dir ./converted

This converts a Supervisor config tree into a native Direktor layout:
  OUTPUT_DIR/direktor.conf
  OUTPUT_DIR/conf.d/*.conf`)
}

// writeOutput renders the main config and one file per program / group into
// outputDir. Filenames go through fileSafeName() because supervisor's name
// rules are looser than what filesystems like.
func writeOutput(outputDir string, raw iniFile, cfg *types.Config) error {
	if err := os.MkdirAll(filepath.Join(outputDir, "conf.d"), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	mainPath := filepath.Join(outputDir, "direktor.conf")
	mainContent := renderMainConfig(outputDir, raw, cfg)
	if err := os.WriteFile(mainPath, []byte(mainContent), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", mainPath, err)
	}

	// Programs first, then groups. Sorted so reruns produce identical output.
	programNames := sortedProgramNames(cfg.Programs)
	for _, name := range programNames {
		path := filepath.Join(outputDir, "conf.d", fileSafeName(name)+".conf")
		content := renderProgramConfig(cfg.Programs[name])
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	groupNames := sortedGroupNames(cfg.Groups)
	for _, name := range groupNames {
		path := filepath.Join(outputDir, "conf.d", "group-"+fileSafeName(name)+".conf")
		content := renderGroupConfig(cfg.Groups[name])
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	return nil
}

// renderMainConfig emits the top-level direktor.conf. Globals first, then
// (if applicable) the [email] block, then the [include] glob if there's
// anything in conf.d to include.
func renderMainConfig(outputDir string, raw iniFile, cfg *types.Config) string {
	var b strings.Builder

	b.WriteString("; Generated by scripts/convert-supervisor\n")
	b.WriteString(fmt.Sprintf("; Source layout converted into %s\n\n", outputDir))
	b.WriteString(renderSupervisorSection(cfg.Supervisor))

	if hasEmailConfig(raw.Sections) || cfg.Supervisor.Email.Enabled {
		b.WriteString("\n")
		b.WriteString(renderEmailSection(cfg.Supervisor.Email))
	}

	if len(cfg.Programs) > 0 || len(cfg.Groups) > 0 {
		b.WriteString("\n[include]\n")
		b.WriteString("files = conf.d/*.conf\n")
	}

	return b.String()
}

// renderSupervisorSection emits the [direktord] block. Note we use the
// new name even though the source was [supervisord] — that's the whole
// point of conversion.
func renderSupervisorSection(cfg types.SupervisorConfig) string {
	lines := []string{
		"[direktord]",
		"logfile = " + cfg.LogFile,
		"loglevel = " + cfg.LogLevel,
		"pidfile = " + cfg.PidFile,
		"nodaemon = " + formatBool(cfg.Nodaemon),
		"minfds = " + strconv.Itoa(cfg.MinFDs),
		"minprocs = " + strconv.Itoa(cfg.MinProcs),
		"identifier = " + cfg.Identifier,
		"socket_path = " + cfg.SocketPath,
	}

	// socket_owner and HTTP auth bits are only emitted when set, so we
	// don't litter the output with empty keys for things people didn't use.
	if cfg.SocketOwner != "" {
		lines = append(lines, "socket_owner = "+cfg.SocketOwner)
	}
	lines = append(lines, "socket_mode = "+cfg.SocketMode)
	lines = append(lines, "http_host = "+cfg.HTTPHost)
	lines = append(lines, "http_port = "+strconv.Itoa(cfg.HTTPPort))
	if cfg.HTTPAuth != "" {
		lines = append(lines, "http_auth = "+cfg.HTTPAuth)
	}
	if cfg.HTTPAuthToken != "" {
		lines = append(lines, "http_auth_token = "+cfg.HTTPAuthToken)
	}

	return strings.Join(lines, "\n") + "\n"
}

// renderEmailSection — same "only emit what's set" philosophy for SMTP.
func renderEmailSection(cfg types.EmailNotifyConfig) string {
	lines := []string{
		"[email]",
		"enabled = " + formatBool(cfg.Enabled),
	}
	if cfg.SMTPHost != "" {
		lines = append(lines, "smtp_host = "+cfg.SMTPHost)
	}
	if cfg.SMTPPort != 0 {
		lines = append(lines, "smtp_port = "+strconv.Itoa(cfg.SMTPPort))
	}
	if cfg.Username != "" {
		lines = append(lines, "username = "+cfg.Username)
	}
	if cfg.Password != "" {
		lines = append(lines, "password = "+cfg.Password)
	}
	if cfg.From != "" {
		lines = append(lines, "from = "+cfg.From)
	}
	if len(cfg.Recipients) > 0 {
		lines = append(lines, "recipients = "+strings.Join(cfg.Recipients, ", "))
	}
	lines = append(lines, "use_tls = "+formatBool(cfg.UseTLS))
	if len(cfg.NotifyOn) > 0 {
		lines = append(lines, "notify_on = "+strings.Join(cfg.NotifyOn, ", "))
	}
	return strings.Join(lines, "\n") + "\n"
}

// renderProgramConfig — one [program:x] block. Long. Repetitive. Deal with it.
func renderProgramConfig(cfg types.ProcessConfig) string {
	lines := []string{
		fmt.Sprintf("[program:%s]", cfg.Name),
		"command = " + cfg.Command,
	}
	if cfg.Directory != "" {
		lines = append(lines, "directory = "+cfg.Directory)
	}
	if cfg.User != "" {
		lines = append(lines, "user = "+cfg.User)
	}
	lines = append(lines,
		"autostart = "+formatBool(cfg.AutoStart),
		"autorestart = "+string(cfg.AutoRestart),
		"startsecs = "+strconv.Itoa(cfg.StartSecs),
		"startretries = "+strconv.Itoa(cfg.StartRetries),
		"stopsignal = "+cfg.StopSignal,
		"stopwaitsecs = "+strconv.Itoa(cfg.StopWaitSecs),
		"exitcodes = "+joinInts(cfg.ExitCodes),
		"priority = "+strconv.Itoa(cfg.Priority),
		"numprocs = "+strconv.Itoa(cfg.NumProcs),
		"numprocs_start = "+strconv.Itoa(cfg.NumProcsStart),
	)
	if cfg.StdoutLogFile != "" {
		lines = append(lines, "stdout_logfile = "+cfg.StdoutLogFile)
	}
	if cfg.StderrLogFile != "" {
		lines = append(lines, "stderr_logfile = "+cfg.StderrLogFile)
	}
	lines = append(lines,
		"stdout_logfile_maxbytes = "+formatBytes(cfg.StdoutLogMaxBytes),
		"stdout_logfile_backups = "+strconv.Itoa(cfg.StdoutLogBackups),
		"stderr_logfile_maxbytes = "+formatBytes(cfg.StderrLogMaxBytes),
		"stderr_logfile_backups = "+strconv.Itoa(cfg.StderrLogBackups),
		"redirect_stderr = "+formatBool(cfg.RedirectStderr),
	)
	if len(cfg.Environment) > 0 {
		lines = append(lines, "environment = "+formatEnvironment(cfg.Environment))
	}
	return strings.Join(lines, "\n") + "\n"
}

// renderGroupConfig — small block, small renderer.
func renderGroupConfig(cfg types.GroupConfig) string {
	lines := []string{
		fmt.Sprintf("[group:%s]", cfg.Name),
		"programs = " + strings.Join(cfg.Programs, ","),
		"priority = " + strconv.Itoa(cfg.Priority),
	}
	return strings.Join(lines, "\n") + "\n"
}

// parseINIWithWarnings is the "raw" walker — gives us section names and
// keys verbatim so we can flag the things the proper parser silently drops.
func parseINIWithWarnings(path string) (iniFile, []warning, error) {
	return parseINIRecursive(path, map[string]bool{})
}

// parseINIRecursive walks include directives. The `visited` map is what
// stops us blowing the stack on a cyclic include — supervisor would do
// the same loop forever, which is on-brand for it but not very helpful.
func parseINIRecursive(path string, visited map[string]bool) (iniFile, []warning, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return iniFile{}, nil, fmt.Errorf("resolve path %s: %w", path, err)
	}
	if visited[path] {
		return iniFile{Sections: make(map[string]map[string]string)}, nil, nil
	}
	visited[path] = true

	f, err := os.Open(path)
	if err != nil {
		return iniFile{}, nil, err
	}
	defer f.Close()

	result := iniFile{Sections: make(map[string]map[string]string)}
	var warnings []warning
	currentSection := ""

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := result.Sections[currentSection]; !ok {
				result.Sections[currentSection] = make(map[string]string)
				result.Order = append(result.Order, currentSection)
			}
			if isUnsupportedSection(currentSection) {
				warnings = append(warnings, warning{
					Path:    path,
					Section: currentSection,
					Reason:  "unsupported section",
				})
			}
			continue
		}

		if currentSection == "" {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if currentSection == "include" && key == "files" {
			// Recurse into included files. The merge happens via mergeINI
			// so the parent's section order is preserved at the top.
			matches, err := resolveInclude(value, filepath.Dir(path))
			if err != nil {
				return iniFile{}, nil, fmt.Errorf("resolve include %q: %w", value, err)
			}
			for _, match := range matches {
				child, childWarnings, err := parseINIRecursive(match, visited)
				if err != nil {
					return iniFile{}, nil, err
				}
				mergeINI(&result, child)
				warnings = append(warnings, childWarnings...)
			}
			result.Sections[currentSection][key] = value
			continue
		}

		result.Sections[currentSection][key] = value
		if !isSupportedKey(currentSection, key) {
			warnings = append(warnings, warning{
				Path:    path,
				Section: currentSection,
				Key:     key,
				Reason:  "unsupported key",
			})
		}
	}

	if err := scanner.Err(); err != nil {
		return iniFile{}, nil, err
	}

	return result, warnings, nil
}

// mergeINI lays src on top of dst in place. New sections go to the end of
// the order list; existing sections get their keys overlayed. Last write wins.
func mergeINI(dst *iniFile, src iniFile) {
	for _, section := range src.Order {
		if _, ok := dst.Sections[section]; !ok {
			dst.Sections[section] = make(map[string]string)
			dst.Order = append(dst.Order, section)
		}
		for key, value := range src.Sections[section] {
			dst.Sections[section][key] = value
		}
	}
}

// resolveInclude expands a glob to an absolute, sorted list of paths.
// Sorted because filepath.Glob doesn't promise an order and we want
// deterministic output.
func resolveInclude(pattern, baseDir string) ([]string, error) {
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(baseDir, pattern)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// isUnsupportedSection — true if the section name isn't one we'd render.
// Anything matching this triggers a warning (we still keep the keys around
// in raw, but they don't end up in the output config).
func isUnsupportedSection(name string) bool {
	switch {
	case name == "supervisord", name == "direktord", name == "email", name == "notify", name == "include":
		return false
	case strings.HasPrefix(name, "program:"), strings.HasPrefix(name, "group:"):
		return false
	default:
		return true
	}
}

// isSupportedKey — true if (section, key) is something we know how to
// translate. Used for the warning pass.
func isSupportedKey(section, key string) bool {
	switch {
	case section == "supervisord", section == "direktord":
		_, ok := supervisorKeys[key]
		return ok
	case section == "email", section == "notify":
		_, ok := emailKeys[key]
		return ok
	case section == "include":
		return key == "files"
	case strings.HasPrefix(section, "program:"):
		_, ok := programKeys[key]
		return ok
	case strings.HasPrefix(section, "group:"):
		_, ok := groupKeys[key]
		return ok
	default:
		return false
	}
}

// printWarnings dumps everything we found to stderr in a sorted, scannable
// shape. Stderr (not stdout) so that you can pipe stdout straight into a
// pager without warnings clogging it up.
func printWarnings(warnings []warning) {
	if len(warnings) == 0 {
		return
	}

	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].Path != warnings[j].Path {
			return warnings[i].Path < warnings[j].Path
		}
		if warnings[i].Section != warnings[j].Section {
			return warnings[i].Section < warnings[j].Section
		}
		return warnings[i].Key < warnings[j].Key
	})

	fmt.Fprintln(os.Stderr, "Warnings:")
	for _, w := range warnings {
		if w.Key != "" {
			fmt.Fprintf(os.Stderr, "  %s [%s] %s: %s\n", w.Path, w.Section, w.Key, w.Reason)
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s [%s]: %s\n", w.Path, w.Section, w.Reason)
	}
}

// hasEmailConfig — did the source actually contain an [email] or [notify]
// section? Used to decide whether to emit the email block in the output
// even if the parsed cfg.Email looks empty (e.g. enabled=false).
func hasEmailConfig(sections map[string]map[string]string) bool {
	_, okEmail := sections["email"]
	_, okNotify := sections["notify"]
	return okEmail || okNotify
}

// sortedProgramNames / sortedGroupNames — yeah, two near-identical helpers.
// Generics would dedupe these but the call sites are clearer with the
// concrete types and there are only two of them. Moving on.
func sortedProgramNames(programs map[string]types.ProcessConfig) []string {
	names := make([]string, 0, len(programs))
	for name := range programs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedGroupNames(groups map[string]types.GroupConfig) []string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// fileSafeName turns a program name into something safe to use as a
// filename on every OS we care about. Lowercase, slashes/colons/spaces
// replaced with hyphens, edges trimmed. Empty input returns "unnamed"
// because writing a file called "" is a great way to break things.
func fileSafeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		":", "-",
	)
	name = replacer.Replace(name)
	name = strings.Trim(name, "-.")
	if name == "" {
		return "unnamed"
	}
	return name
}

func formatBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func joinInts(values []int) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
}

// formatBytes is the inverse of config.parseBytes — emits "10MB" / "2GB" /
// etc. when the number divides cleanly, and a plain integer otherwise.
// Cleanly-divides means "%/(unit) == 0", so 10485760 becomes "10MB" but
// 10485761 stays as the raw number.
func formatBytes(v int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case v == 0:
		return "0"
	case v%gb == 0:
		return strconv.FormatInt(v/gb, 10) + "GB"
	case v%mb == 0:
		return strconv.FormatInt(v/mb, 10) + "MB"
	case v%kb == 0:
		return strconv.FormatInt(v/kb, 10) + "KB"
	default:
		return strconv.FormatInt(v, 10)
	}
}

// formatEnvironment renders an env map back into the supervisor wire format
// (`KEY="val",KEY2="val2"`). Keys are sorted so the output's deterministic.
func formatEnvironment(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, key, escapeEnvValue(env[key])))
	}
	return strings.Join(parts, ",")
}

// escapeEnvValue does the bare-minimum quoting for values going into the
// supervisor environment syntax — backslashes and double quotes. Doesn't
// handle commas, which is the limitation called out in config.parseEnvironment.
func escapeEnvValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

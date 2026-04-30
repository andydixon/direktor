// Package config parses the INI-flavoured configuration files that Direktor
// (and its predecessor, Supervisor) uses.
//
// We accept the supervisord dialect verbatim — `[program:foo]`, `[group:bar]`,
// `[include]`, the lot — because that's the whole migration story. You point
// us at your existing supervisord.conf, we read it, you wonder why you spent
// years putting up with the Python one. That's the dream, anyway.
//
// New configs should use `[direktord]` instead of `[supervisord]` for the
// global block, but both are accepted and they mean the same thing. We don't
// fight people on naming.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/andydixon/direktor/pkg/types"
)

// Section header, key=value, and the include-files regex.
//
// Yes, we could write a "proper" parser. We don't need one. INI is genuinely
// simple if you ignore the parts of the spec nobody actually uses. Three
// regexes and a scanner sees us right.
var (
	sectionRe  = regexp.MustCompile(`^\[(.+)\]$`)
	keyValueRe = regexp.MustCompile(`^([^=]+?)\s*=\s*(.*)$`)
	includeRe  = regexp.MustCompile(`^files\s*=\s*(.+)$`)
)

// Parse reads a supervisord/direktord-style INI file and returns a fully
// populated *types.Config. Anything missing from the file is filled in from
// the defaults — see defaultSupervisorConfig and defaultProcessConfig — so
// callers can rely on a complete struct coming back.
//
// Errors here are about *parsing* (bad file, broken include path, etc.).
// Validation of values (e.g. "is /var/log/this writable?") happens later
// when we actually try to use them.
func Parse(path string) (*types.Config, error) {
	sections, err := parseINI(path)
	if err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg := &types.Config{
		Programs: make(map[string]types.ProcessConfig),
		Groups:   make(map[string]types.GroupConfig),
	}

	cfg.Supervisor = defaultSupervisorConfig()

	// Walk every section and dispatch to the appropriate parser. The
	// section *prefix* tells us what kind of thing it is. Order doesn't
	// matter — INI is unordered as far as we're concerned.
	for name, kvs := range sections {
		switch {
		case name == "supervisord" || name == "direktord":
			parseSupervisorSection(kvs, &cfg.Supervisor)
		case name == "email" || name == "notify":
			// "notify" is the slightly nicer name. "email" is the one that
			// matches what the section actually does. Both work.
			parseEmailSection(kvs, &cfg.Supervisor.Email)
		case strings.HasPrefix(name, "program:"):
			progName := strings.TrimPrefix(name, "program:")
			pc := defaultProcessConfig(progName)
			parseProcessSection(kvs, &pc)
			cfg.Programs[progName] = pc
		case strings.HasPrefix(name, "group:"):
			groupName := strings.TrimPrefix(name, "group:")
			gc := types.GroupConfig{Name: groupName}
			parseGroupSection(kvs, &gc)
			cfg.Groups[groupName] = gc
		}
		// Unknown sections fall through silently. The convert-supervisor
		// helper warns about those — at runtime we'd rather not get in
		// the way.
	}

	return cfg, nil
}

// defaultSupervisorConfig is the "I gave you nothing, you start somewhere
// reasonable" config. The numbers are chosen to match what Supervisor users
// expect, with the obvious path adjustments (we live under /var/log/direktor,
// not /var/log/supervisor).
func defaultSupervisorConfig() types.SupervisorConfig {
	socketPath := "/var/run/direktor.sock"
	if runtime.GOOS == "windows" {
		// Windows doesn't do unix sockets; the IPC layer falls back to
		// loopback TCP, but the path lives here for future named-pipe work.
		socketPath = `\\.\pipe\direktor`
	}
	return types.SupervisorConfig{
		LogFile:    "/var/log/direktor/direktord.log",
		LogLevel:   "info",
		PidFile:    "/var/run/direktor.pid",
		Nodaemon:   false,
		MinFDs:     1024,
		MinProcs:   200,
		Identifier: "direktor",
		SocketPath: socketPath,
		SocketMode: "0700",
		HTTPPort:   9876,
		HTTPHost:   "127.0.0.1", // localhost only by default — opt in to exposing it
	}
}

// defaultProcessConfig sets the "didn't say, so we picked sensible" values
// for a program block. Note autostart=true and autorestart=always: if you
// took the trouble to define a program, you almost certainly want it
// running.
func defaultProcessConfig(name string) types.ProcessConfig {
	return types.ProcessConfig{
		Name:              name,
		AutoStart:         true,
		AutoRestart:       types.RestartAlways,
		StartSecs:         1,
		StartRetries:      3,
		StopSignal:        "TERM",
		StopWaitSecs:      10,
		ExitCodes:         []int{0},
		Priority:          999,
		NumProcs:          1,
		NumProcsStart:     0,
		StdoutLogMaxBytes: 50 * 1024 * 1024, // 50MB — generous enough you won't notice rotation
		StderrLogMaxBytes: 50 * 1024 * 1024,
		StdoutLogBackups:  10,
		StderrLogBackups:  10,
		Environment:       make(map[string]string),
	}
}

// parseSupervisorSection — the global daemon settings. Mostly straight string
// copies; the integers get a `_, _ = strconv.Atoi(...)` because if you wrote
// "twelve" instead of "12" we'd rather use the default than refuse to start.
func parseSupervisorSection(kvs map[string]string, cfg *types.SupervisorConfig) {
	if v, ok := kvs["logfile"]; ok {
		cfg.LogFile = v
	}
	if v, ok := kvs["loglevel"]; ok {
		cfg.LogLevel = v
	}
	if v, ok := kvs["pidfile"]; ok {
		cfg.PidFile = v
	}
	if v, ok := kvs["nodaemon"]; ok {
		cfg.Nodaemon = parseBool(v)
	}
	if v, ok := kvs["minfds"]; ok {
		cfg.MinFDs, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["minprocs"]; ok {
		cfg.MinProcs, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["identifier"]; ok {
		cfg.Identifier = v
	}
	if v, ok := kvs["socket_path"]; ok {
		cfg.SocketPath = v
	}
	if v, ok := kvs["socket_mode"]; ok {
		cfg.SocketMode = v
	}
	if v, ok := kvs["http_port"]; ok {
		cfg.HTTPPort, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["http_host"]; ok {
		cfg.HTTPHost = v
	}
	if v, ok := kvs["http_auth"]; ok {
		cfg.HTTPAuth = v
	}
	if v, ok := kvs["http_auth_token"]; ok {
		cfg.HTTPAuthToken = v
	}
}

// parseProcessSection — the [program:x] block. Long, repetitive, hard to make
// less long without sacrificing clarity. So it's long.
func parseProcessSection(kvs map[string]string, pc *types.ProcessConfig) {
	if v, ok := kvs["command"]; ok {
		pc.Command = v
	}
	if v, ok := kvs["directory"]; ok {
		pc.Directory = v
	}
	if v, ok := kvs["user"]; ok {
		pc.User = v
	}
	if v, ok := kvs["autostart"]; ok {
		pc.AutoStart = parseBool(v)
	}
	if v, ok := kvs["autorestart"]; ok {
		// Supervisor used "unexpected" for what everyone else calls
		// "on-failure". We accept both; you're welcome.
		switch v {
		case "true", "always":
			pc.AutoRestart = types.RestartAlways
		case "false", "never":
			pc.AutoRestart = types.RestartNever
		case "unexpected", "on-failure":
			pc.AutoRestart = types.RestartOnFailure
		}
	}
	if v, ok := kvs["startsecs"]; ok {
		pc.StartSecs, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["startretries"]; ok {
		pc.StartRetries, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["stopsignal"]; ok {
		pc.StopSignal = strings.ToUpper(v)
	}
	if v, ok := kvs["stopwaitsecs"]; ok {
		pc.StopWaitSecs, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["exitcodes"]; ok {
		pc.ExitCodes = parseIntList(v)
	}
	if v, ok := kvs["priority"]; ok {
		pc.Priority, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["numprocs"]; ok {
		pc.NumProcs, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["numprocs_start"]; ok {
		pc.NumProcsStart, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["stdout_logfile"]; ok {
		pc.StdoutLogFile = v
	}
	if v, ok := kvs["stderr_logfile"]; ok {
		pc.StderrLogFile = v
	}
	if v, ok := kvs["stdout_logfile_maxbytes"]; ok {
		pc.StdoutLogMaxBytes = parseBytes(v)
	}
	if v, ok := kvs["stderr_logfile_maxbytes"]; ok {
		pc.StderrLogMaxBytes = parseBytes(v)
	}
	if v, ok := kvs["stdout_logfile_backups"]; ok {
		pc.StdoutLogBackups, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["stderr_logfile_backups"]; ok {
		pc.StderrLogBackups, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["redirect_stderr"]; ok {
		pc.RedirectStderr = parseBool(v)
	}
	if v, ok := kvs["environment"]; ok {
		pc.Environment = parseEnvironment(v)
	}
}

// parseGroupSection — small block, small parser. Programs is a comma-list,
// priority is the number.
func parseGroupSection(kvs map[string]string, gc *types.GroupConfig) {
	if v, ok := kvs["programs"]; ok {
		parts := strings.Split(v, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				gc.Programs = append(gc.Programs, p)
			}
		}
	}
	if v, ok := kvs["priority"]; ok {
		gc.Priority, _ = strconv.Atoi(v)
	}
}

// parseEmailSection — SMTP details for the notifier.
//
// notify_on is uppercased so we can compare it directly to types.ProcessState
// values without playing case-insensitive games later.
func parseEmailSection(kvs map[string]string, ec *types.EmailNotifyConfig) {
	if v, ok := kvs["enabled"]; ok {
		ec.Enabled = parseBool(v)
	}
	if v, ok := kvs["smtp_host"]; ok {
		ec.SMTPHost = v
	}
	if v, ok := kvs["smtp_port"]; ok {
		ec.SMTPPort, _ = strconv.Atoi(v)
	}
	if v, ok := kvs["username"]; ok {
		ec.Username = v
	}
	if v, ok := kvs["password"]; ok {
		ec.Password = v
	}
	if v, ok := kvs["from"]; ok {
		ec.From = v
	}
	if v, ok := kvs["recipients"]; ok {
		for _, r := range strings.Split(v, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				ec.Recipients = append(ec.Recipients, r)
			}
		}
	}
	if v, ok := kvs["use_tls"]; ok {
		ec.UseTLS = parseBool(v)
	}
	if v, ok := kvs["notify_on"]; ok {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(strings.ToUpper(s))
			if s != "" {
				ec.NotifyOn = append(ec.NotifyOn, s)
			}
		}
	}
}

// parseINI walks the file line by line and builds up a section -> kv map.
// It also follows [include] files = ... directives recursively.
//
// One thing to flag: included files are merged in, *and they win* on key
// collisions. That matches Supervisor's behaviour and seems to be what
// people expect. If you don't like it, don't have collisions.
func parseINI(path string) (map[string]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sections := make(map[string]map[string]string)
	currentSection := ""

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and blanks. We accept both `;` (Supervisor / Python
		// configparser style) and `#` (everyone else's style) because life's
		// too short to care which one you typed.
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}

		// Section header: [name]
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			currentSection = m[1]
			if _, exists := sections[currentSection]; !exists {
				sections[currentSection] = make(map[string]string)
			}
			continue
		}

		// key = value
		if m := keyValueRe.FindStringSubmatch(line); m != nil {
			key := strings.TrimSpace(m[1])
			value := strings.TrimSpace(m[2])

			if currentSection == "include" && key == "files" {
				// Include directive — go off, parse the matched files,
				// merge their sections into ours.
				included, err := handleInclude(value, filepath.Dir(path))
				if err != nil {
					return nil, fmt.Errorf("processing include %q: %w", value, err)
				}
				for sName, sKVs := range included {
					if _, exists := sections[sName]; !exists {
						sections[sName] = make(map[string]string)
					}
					for k, v := range sKVs {
						sections[sName][k] = v
					}
				}
			} else if currentSection != "" {
				sections[currentSection][key] = value
			}
			// Keys outside any section get silently ignored. They're nearly
			// always typos.
		}
	}

	return sections, scanner.Err()
}

// handleInclude resolves a glob pattern (relative paths are relative to the
// file doing the including, not to wherever direktord was launched from —
// supervisor got that wrong and it bit people) and parses everything that
// matches.
func handleInclude(pattern, baseDir string) (map[string]map[string]string, error) {
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(baseDir, pattern)
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	result := make(map[string]map[string]string)
	for _, match := range matches {
		sections, err := parseINI(match)
		if err != nil {
			return nil, fmt.Errorf("parsing included file %s: %w", match, err)
		}
		for sName, sKVs := range sections {
			if _, exists := result[sName]; !exists {
				result[sName] = make(map[string]string)
			}
			for k, v := range sKVs {
				result[sName][k] = v
			}
		}
	}

	return result, nil
}

// parseBool — accepts the things people actually type. Anything else is false.
// We don't error on garbage; defaulting to false is almost always less
// surprising than refusing to start.
func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes"
}

// parseIntList — comma-separated ints. Bad entries are skipped, not errored.
// Same philosophy as parseBool: be permissive, don't refuse to boot over a
// stray space.
func parseIntList(s string) []int {
	var result []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if v, err := strconv.Atoi(part); err == nil {
			result = append(result, v)
		}
	}
	return result
}

// parseBytes accepts "10MB", "500KB", "2GB", or a plain number (interpreted
// as bytes). Case-insensitive on the suffix because nobody can ever remember
// whether it's "MB" or "Mb" or "mb".
//
// And yes, we use 1024-based units (so "MB" really means MiB). That's what
// supervisor did, that's what every log-rotation tool on Linux does, and
// arguing about SI prefixes in 2026 is a fight nobody needs.
func parseBytes(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	multiplier := int64(1)

	if strings.HasSuffix(s, "MB") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	} else if strings.HasSuffix(s, "KB") {
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	} else if strings.HasSuffix(s, "GB") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	}

	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v * multiplier
}

// parseEnvironment turns supervisor-style `KEY="val",KEY2="val2"` into a map.
//
// This is a bit shit, frankly — supervisor's environment syntax doesn't deal
// well with values containing commas (think `PATH=/a:/b,c:/d`) and we inherit
// that limitation. But it's what existing configs use, so we honour it. If
// you've got a value with embedded commas, set it via [program:x] using a
// wrapper script and save us all the heartache.
func parseEnvironment(s string) map[string]string {
	env := make(map[string]string)
	pairs := strings.Split(s, ",")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`) // strip a single layer of quotes
			env[key] = val
		}
	}
	return env
}

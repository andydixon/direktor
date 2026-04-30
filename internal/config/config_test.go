package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	content := `
[supervisord]
logfile = /tmp/direktor.log
loglevel = debug
pidfile = /tmp/direktor.pid
nodaemon = true

[program:myapp]
command = /usr/bin/myapp --flag
directory = /opt/myapp
autostart = true
autorestart = unexpected
startsecs = 5
startretries = 3
stopsignal = TERM
stopwaitsecs = 10
exitcodes = 0,2
stdout_logfile = /var/log/myapp/stdout.log
stderr_logfile = /var/log/myapp/stderr.log
stdout_logfile_maxbytes = 10MB
environment = PATH="/usr/bin",HOME="/root"
priority = 100
user = nobody

[group:webapps]
programs = myapp,otherapp
priority = 100
`

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "direktor.conf")
	if err := os.WriteFile(cfgFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse(cfgFile)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Check supervisor config
	if cfg.Supervisor.LogFile != "/tmp/direktor.log" {
		t.Errorf("LogFile = %q, want /tmp/direktor.log", cfg.Supervisor.LogFile)
	}
	if cfg.Supervisor.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.Supervisor.LogLevel)
	}
	if !cfg.Supervisor.Nodaemon {
		t.Error("Nodaemon should be true")
	}

	// Check program config
	prog, ok := cfg.Programs["myapp"]
	if !ok {
		t.Fatal("program 'myapp' not found")
	}
	if prog.Command != "/usr/bin/myapp --flag" {
		t.Errorf("Command = %q", prog.Command)
	}
	if prog.Directory != "/opt/myapp" {
		t.Errorf("Directory = %q", prog.Directory)
	}
	if prog.StartSecs != 5 {
		t.Errorf("StartSecs = %d, want 5", prog.StartSecs)
	}
	if prog.StdoutLogMaxBytes != 10*1024*1024 {
		t.Errorf("StdoutLogMaxBytes = %d, want %d", prog.StdoutLogMaxBytes, 10*1024*1024)
	}
	if prog.Environment["PATH"] != "/usr/bin" {
		t.Errorf("Environment PATH = %q", prog.Environment["PATH"])
	}
	if prog.Priority != 100 {
		t.Errorf("Priority = %d, want 100", prog.Priority)
	}
	if len(prog.ExitCodes) != 2 || prog.ExitCodes[0] != 0 || prog.ExitCodes[1] != 2 {
		t.Errorf("ExitCodes = %v, want [0 2]", prog.ExitCodes)
	}

	// Check group config
	grp, ok := cfg.Groups["webapps"]
	if !ok {
		t.Fatal("group 'webapps' not found")
	}
	if len(grp.Programs) != 2 {
		t.Errorf("Group programs = %v, want 2 entries", grp.Programs)
	}
}

func TestParseInclude(t *testing.T) {
	dir := t.TempDir()

	// Create an included file
	included := `
[program:worker]
command = /usr/bin/worker
autostart = true
`
	incDir := filepath.Join(dir, "conf.d")
	os.MkdirAll(incDir, 0755)
	os.WriteFile(filepath.Join(incDir, "worker.conf"), []byte(included), 0644)

	// Create main config
	main := `
[supervisord]
logfile = /tmp/test.log

[include]
files = conf.d/*.conf
`
	mainFile := filepath.Join(dir, "direktor.conf")
	os.WriteFile(mainFile, []byte(main), 0644)

	cfg, err := Parse(mainFile)
	if err != nil {
		t.Fatalf("Parse with include failed: %v", err)
	}

	if _, ok := cfg.Programs["worker"]; !ok {
		t.Error("included program 'worker' not found")
	}
}

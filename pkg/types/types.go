// Package types holds the shared types that every other package in Direktor
// ends up reaching for. It's deliberately tiny — no logic, no I/O, just structs
// and the odd enum, so anyone can import it without dragging the world in with
// them.
//
// Most of the field names are kept deliberately close to Supervisor's old
// vocabulary (autostart, startsecs, exitcodes, etc.) so configs and tooling
// from supervisord land here without the whole world needing rewriting. We
// don't *love* the names — startsecs in particular reads like a typo — but
// migration is hard enough as it is.
package types

import "time"

// ProcessState is the state machine label for a managed process. Supervisor's
// lifecycle (STOPPED → STARTING → RUNNING → ...) is well-trodden territory, so
// we kept it. It's the one thing it got reasonably right.
type ProcessState string

// The lifecycle. Read from top to bottom — STOPPED is the resting state, FATAL
// is "we've given up, it kept dying, you sort it out". UNKNOWN is the "we
// genuinely don't know" escape hatch and should basically never appear.
const (
	StateStopped  ProcessState = "STOPPED"
	StateStarting ProcessState = "STARTING"
	StateRunning  ProcessState = "RUNNING"
	StateBackoff  ProcessState = "BACKOFF"  // crashed too fast, having a sit-down
	StateStopping ProcessState = "STOPPING" // we asked nicely, waiting for it
	StateExited   ProcessState = "EXITED"
	StateFatal    ProcessState = "FATAL" // out of retries, no more chances
	StateUnknown  ProcessState = "UNKNOWN"
)

// AutoRestartPolicy decides whether a dead process gets resuscitated.
//
// Supervisor used "unexpected" instead of "on-failure" which is — fine, I
// suppose, but every other supervisor on the planet calls it on-failure, so
// we accept both at parse time and normalise to this.
type AutoRestartPolicy string

const (
	RestartAlways    AutoRestartPolicy = "always"
	RestartOnFailure AutoRestartPolicy = "on-failure"
	RestartNever     AutoRestartPolicy = "never"
)

// ProcessInfo is the runtime snapshot of a process — what the CLI, web UI and
// IPC clients all want to render. It's a snapshot: by the time you read it the
// PID may already be a corpse. Don't rely on it for life-or-death decisions.
type ProcessInfo struct {
	Name        string       `json:"name"`
	Group       string       `json:"group"`
	State       ProcessState `json:"state"`
	Description string       `json:"description"` // human-readable "pid 1234, uptime 2h"
	PID         int          `json:"pid"`
	ExitCode    int          `json:"exit_code"`
	StartTime   time.Time    `json:"start_time"`
	StopTime    time.Time    `json:"stop_time"`
	Uptime      string       `json:"uptime"`
	LogFile     string       `json:"log_file"`
	StderrLog   string       `json:"stderr_log"`
}

// ProcessConfig is everything the user wrote in a [program:x] block, parsed
// into a struct. Defaults are filled in by config.defaultProcessConfig — try
// not to put zero-values here; an unset StartRetries of 0 would make us give
// up on the first wobble, which is rude.
type ProcessConfig struct {
	Name              string            `json:"name"`
	Command           string            `json:"command"`
	Directory         string            `json:"directory"`
	User              string            `json:"user"`
	Environment       map[string]string `json:"environment"`
	AutoStart         bool              `json:"autostart"`
	AutoRestart       AutoRestartPolicy `json:"autorestart"`
	StartSecs         int               `json:"startsecs"`     // seconds it must stay up to count as "running"
	StartRetries      int               `json:"startretries"`  // before we throw our hands up and go FATAL
	StopSignal        string            `json:"stopsignal"`
	StopWaitSecs      int               `json:"stopwaitsecs"`  // grace period before we go nuclear (SIGKILL)
	ExitCodes         []int             `json:"exitcodes"`     // exit codes that count as "expected"
	Priority          int               `json:"priority"`
	NumProcs          int               `json:"numprocs"`
	NumProcsStart     int               `json:"numprocs_start"`
	StdoutLogFile     string            `json:"stdout_logfile"`
	StderrLogFile     string            `json:"stderr_logfile"`
	StdoutLogMaxBytes int64             `json:"stdout_logfile_maxbytes"`
	StderrLogMaxBytes int64             `json:"stderr_logfile_maxbytes"`
	StdoutLogBackups  int               `json:"stdout_logfile_backups"`
	StderrLogBackups  int               `json:"stderr_logfile_backups"`
	RedirectStderr    bool              `json:"redirect_stderr"`
}

// GroupConfig — a [group:x] block. Mostly a label and a list of programs you
// can act on together. Honestly, in 2026 most people use systemd targets for
// this, but Supervisor users love their groups, so here they are.
type GroupConfig struct {
	Name     string   `json:"name"`
	Programs []string `json:"programs"`
	Priority int      `json:"priority"`
}

// SupervisorConfig is the global [supervisord] / [direktord] block. Yes, both
// names are accepted at parse time — see config.Parse. Keeping the supervisord
// name working means a `cp` is a valid migration step, which matters more than
// stylistic purity.
type SupervisorConfig struct {
	LogFile       string `json:"logfile"`
	LogLevel      string `json:"loglevel"`
	PidFile       string `json:"pidfile"`
	Nodaemon      bool   `json:"nodaemon"`
	MinFDs        int    `json:"minfds"`   // mostly cosmetic, we don't actually setrlimit (yet)
	MinProcs      int    `json:"minprocs"` // ditto
	Identifier    string `json:"identifier"`
	SocketPath    string `json:"socket_path"`
	SocketOwner   string `json:"socket_owner"`
	SocketMode    string `json:"socket_mode"`
	HTTPPort      int    `json:"http_port"`
	HTTPHost      string `json:"http_host"`
	HTTPAuth      string `json:"http_auth"`       // "", "token" or "basic"
	HTTPAuthToken string `json:"http_auth_token"` // shared secret / basic-auth password
	Email         EmailNotifyConfig `json:"email"`
}

// EmailNotifyConfig — SMTP knobs for the notifier. Optional; if Enabled is
// false the whole notifier subsystem just sits there harmlessly. Supervisor
// needed a third-party event listener plugin to do this, which was a faff and
// a half. Built-in seemed kinder.
type EmailNotifyConfig struct {
	Enabled    bool     `json:"enabled"`
	SMTPHost   string   `json:"smtp_host"`
	SMTPPort   int      `json:"smtp_port"`
	Username   string   `json:"username"`
	Password   string   `json:"password"`
	From       string   `json:"from"`
	Recipients []string `json:"recipients"`
	UseTLS     bool     `json:"use_tls"`
	NotifyOn   []string `json:"notify_on"` // states that trigger an email; empty = sensible defaults
}

// Config is the whole parsed configuration tree — supervisor-level bits, all
// the [program:x] blocks, all the [group:x] blocks. One of these per running
// daemon.
type Config struct {
	Supervisor SupervisorConfig         `json:"supervisor"`
	Programs   map[string]ProcessConfig `json:"programs"`
	Groups     map[string]GroupConfig   `json:"groups"`
}

// Command is what direktorctl (or anything else speaking IPC) sends down the
// wire. Action is a verb ("start", "stop", "status", ...), Args is whatever
// goes after.
type Command struct {
	Action string   `json:"action"`
	Args   []string `json:"args"`
}

// Response is the reply. Success is the boolean you check first; Message is
// human-facing; Data is whatever structured payload the action wanted to
// return (a ProcessInfo, a slice of them, or nothing).
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

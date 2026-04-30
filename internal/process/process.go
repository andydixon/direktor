// Package process is where the actual "watch a thing, restart it when it
// dies" logic lives. The supervisor package coordinates many of these; this
// package just knows how to drive *one* process through its lifecycle.
//
// State machine, roughly:
//
//	STOPPED → STARTING → RUNNING → EXITED → (maybe STARTING again, maybe FATAL)
//	                  ↘ BACKOFF (crashed too quickly) → STARTING → ...
//	                  ↘ STOPPING → STOPPED              (when we asked it to stop)
//
// Every transition fires a callback (StateChangeFunc) — that's how the
// notifier gets to know about things, and how the web UI polls without
// needing to. Supervisor's event listener thing was an XML-RPC nightmare;
// a plain Go callback covers 95% of what people actually wanted.
package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/andydixon/direktor/internal/logging"
	"github.com/andydixon/direktor/pkg/types"
)

// StateChangeFunc is the callback shape used for "something happened to this
// process". Fired on every transition. Don't block in here — we run them in a
// goroutine to be safe but a slow callback is still a smell.
type StateChangeFunc func(name string, oldState, newState types.ProcessState, pid int, exitCode int)

// Process owns one managed thing — config, current state, the os/exec.Cmd
// while it's running, log writers, the lot. Everything that can race is
// behind mu; everything that's set once at construction (config, logger) is
// safe to read without it.
type Process struct {
	mu sync.RWMutex

	config    types.ProcessConfig
	state     types.ProcessState
	cmd       *exec.Cmd
	pid       int
	exitCode  int
	startTime time.Time
	stopTime  time.Time
	retries   int // consecutive quick-exit count, reset once it actually stays up

	stdoutWriter io.WriteCloser
	stderrWriter io.WriteCloser

	cancelFunc    context.CancelFunc // cancels the exec.CommandContext we're driving
	done          chan struct{}      // closed by monitor() when the child has truly exited
	logger        *logging.Logger
	onStateChange StateChangeFunc
}

// New creates a Process in the STOPPED state. It doesn't start anything —
// call Start when you actually want it running.
func New(cfg types.ProcessConfig, logger *logging.Logger) *Process {
	return &Process{
		config: cfg,
		state:  types.StateStopped,
		done:   make(chan struct{}),
		logger: logger,
	}
}

// SetStateChangeCallback registers (or replaces) the thing we call on every
// state transition. Singleton — last one wins, set it once and leave it.
func (p *Process) SetStateChangeCallback(fn StateChangeFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onStateChange = fn
}

// emitStateChange fires the registered callback, if any, in its own goroutine
// so a slow notifier can't wedge the state machine.
func (p *Process) emitStateChange(oldState, newState types.ProcessState) {
	if fn := p.onStateChange; fn != nil {
		go fn(p.config.Name, oldState, newState, p.pid, p.exitCode)
	}
}

// Name — what it's called in the config. Read-only after construction so no
// locking required.
func (p *Process) Name() string {
	return p.config.Name
}

// Config returns a snapshot of the current config (cheap, struct copy).
// Snapshot semantics are deliberate: callers shouldn't be mutating a live
// process's config directly.
func (p *Process) Config() types.ProcessConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config
}

// UpdateConfig swaps in a new config. Doesn't restart the process — most
// fields only take effect on the next start. This is what reload uses.
func (p *Process) UpdateConfig(cfg types.ProcessConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = cfg
}

// State returns the current ProcessState.
func (p *Process) State() types.ProcessState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// Info returns a populated ProcessInfo — what callers (CLI, web UI) want for
// display. The Description field is built here so every consumer gets the
// same human-readable string instead of inventing their own.
func (p *Process) Info() types.ProcessInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	info := types.ProcessInfo{
		Name:      p.config.Name,
		State:     p.state,
		PID:       p.pid,
		ExitCode:  p.exitCode,
		StartTime: p.startTime,
		StopTime:  p.stopTime,
		LogFile:   p.config.StdoutLogFile,
		StderrLog: p.config.StderrLogFile,
	}

	if p.state == types.StateRunning && !p.startTime.IsZero() {
		uptime := time.Since(p.startTime)
		info.Uptime = formatDuration(uptime)
		info.Description = fmt.Sprintf("pid %d, uptime %s", p.pid, info.Uptime)
	} else if p.state == types.StateStopped || p.state == types.StateExited {
		if !p.stopTime.IsZero() {
			info.Description = fmt.Sprintf("exited %s (exit code %d)", p.stopTime.Format(time.RFC3339), p.exitCode)
		}
	}

	return info
}

// Start kicks the process off. Idempotent-ish: if it's already starting or
// running, you get an error, not a second copy. Supervisor would, in some
// edge cases, happily fire up a second copy of your process. We do not.
func (p *Process) Start() error {
	p.mu.Lock()

	if p.state == types.StateRunning || p.state == types.StateStarting {
		p.mu.Unlock()
		return fmt.Errorf("process %s is already running", p.config.Name)
	}

	p.state = types.StateStarting
	p.mu.Unlock()

	return p.spawn()
}

// Stop asks the process to stop. Sends the configured signal first; if it
// hasn't gone away within StopWaitSecs we escalate to SIGKILL. terminate()
// does the actual deed.
func (p *Process) Stop() error {
	p.mu.Lock()

	if p.state != types.StateRunning && p.state != types.StateStarting {
		p.mu.Unlock()
		return fmt.Errorf("process %s is not running (state: %s)", p.config.Name, p.state)
	}

	p.state = types.StateStopping
	p.mu.Unlock()

	return p.terminate()
}

// Restart = stop + start. With a generous timeout on the stop bit because
// sometimes things take a moment to wind down (database connections,
// in-flight requests, etc.).
func (p *Process) Restart() error {
	state := p.State()
	if state == types.StateRunning || state == types.StateStarting {
		if err := p.Stop(); err != nil {
			return err
		}
		// Wait for the previous incarnation to actually exit before
		// firing up a new one. Otherwise you can end up with two copies
		// briefly fighting over the same port.
		select {
		case <-p.done:
		case <-time.After(time.Duration(p.config.StopWaitSecs+2) * time.Second):
			return fmt.Errorf("timeout waiting for process %s to stop", p.config.Name)
		}
	}
	return p.Start()
}

// spawn does the actual fork-and-exec dance. Sets up logging, environment,
// platform attributes, and starts the monitor goroutine that watches for the
// child's death.
//
// This function is shared between "user pressed start" and "auto-restart
// after exit" — that's why it's a separate method, not inlined into Start.
func (p *Process) spawn() error {
	p.mu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFunc = cancel
	p.done = make(chan struct{})

	args := parseCommand(p.config.Command)
	if len(args) == 0 {
		// You wrote `command =` with nothing after it. Or whitespace. Either
		// way we can't run "nothing" — straight to FATAL.
		p.state = types.StateFatal
		p.mu.Unlock()
		cancel()
		return fmt.Errorf("empty command for process %s", p.config.Name)
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)

	// Working directory — entirely optional, child inherits ours if unset.
	if p.config.Directory != "" {
		cmd.Dir = p.config.Directory
	}

	// Environment: if the user specified any vars, start from our own
	// environment and layer theirs on top. If they didn't, the child
	// inherits ours unmodified (cmd.Env=nil = "use parent's env").
	if len(p.config.Environment) > 0 {
		cmd.Env = os.Environ()
		for k, v := range p.config.Environment {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Set up stdout. If this fails — disk full, permissions, whatever —
	// we go straight to FATAL because there's no point starting something
	// whose output we can't capture.
	stdoutWriter, err := p.setupLogWriter(p.config.StdoutLogFile, p.config.StdoutLogMaxBytes, p.config.StdoutLogBackups, "stdout")
	if err != nil {
		p.state = types.StateFatal
		p.mu.Unlock()
		cancel()
		return fmt.Errorf("setting up stdout log: %w", err)
	}
	p.stdoutWriter = stdoutWriter
	cmd.Stdout = stdoutWriter

	// Stderr — either redirected to stdout, or its own writer.
	if p.config.RedirectStderr {
		cmd.Stderr = stdoutWriter
	} else {
		stderrWriter, err := p.setupLogWriter(p.config.StderrLogFile, p.config.StderrLogMaxBytes, p.config.StderrLogBackups, "stderr")
		if err != nil {
			p.state = types.StateFatal
			p.mu.Unlock()
			cancel()
			return fmt.Errorf("setting up stderr log: %w", err)
		}
		p.stderrWriter = stderrWriter
		cmd.Stderr = stderrWriter
	}

	// Platform-specific bits — process group on Unix, console-control on
	// Windows. The implementations are in process_unix.go / process_windows.go.
	setPlatformAttributes(cmd, p.config)

	if err := cmd.Start(); err != nil {
		p.state = types.StateFatal
		p.mu.Unlock()
		cancel()
		return fmt.Errorf("starting process %s: %w", p.config.Name, err)
	}

	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.startTime = time.Now()
	p.exitCode = 0
	p.mu.Unlock()

	p.logger.Info("process started",
		"name", p.config.Name,
		"pid", p.pid,
		"command", p.config.Command,
	)

	// Startup timer. The semantics: a process is only "RUNNING" after it's
	// stayed alive for StartSecs seconds. Until then it's STARTING. This is
	// how we tell "started fine" from "exited immediately, looked alive for
	// a moment". Supervisor invented this convention and, credit where due,
	// it's actually a good one.
	startSecs := p.config.StartSecs
	go func() {
		if startSecs <= 0 {
			// StartSecs=0 means "trust me, it's up the moment it spawns".
			// Mostly useful for short-lived oneshot-ish things.
			p.mu.Lock()
			if p.state == types.StateStarting {
				p.state = types.StateRunning
				p.mu.Unlock()
				p.emitStateChange(types.StateStarting, types.StateRunning)
			} else {
				p.mu.Unlock()
			}
			return
		}
		timer := time.NewTimer(time.Duration(startSecs) * time.Second)
		select {
		case <-timer.C:
			// Made it to StartSecs without dying — congratulations, you're RUNNING.
			p.mu.Lock()
			if p.state == types.StateStarting {
				p.state = types.StateRunning
				p.mu.Unlock()
				p.emitStateChange(types.StateStarting, types.StateRunning)
			} else {
				p.mu.Unlock()
			}
		case <-p.done:
			// Died before the timer fired. monitor() handles the state
			// transition; we just stop the timer.
			timer.Stop()
		}
	}()

	// And the watchdog goroutine — this is what notices the child has
	// exited and decides whether to restart, back off, or give up.
	go p.monitor(ctx)

	return nil
}

// monitor blocks on cmd.Wait() and then decides what happens next: stopped
// cleanly, restart, back off, or escalate to FATAL.
//
// This is the busiest function in the file, and the one most likely to need
// re-reading at 2am when something's misbehaving. Comments are deliberately
// thick.
func (p *Process) monitor(ctx context.Context) {
	defer close(p.done)

	// Block until the child actually exits.
	err := p.cmd.Wait()

	p.mu.Lock()
	p.stopTime = time.Now()

	// Work out the exit code. exec.ExitError gives us the real one;
	// other errors (couldn't even wait?) are -1, which is our "we
	// don't know" sentinel.
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			p.exitCode = exitErr.ExitCode()
		} else {
			p.exitCode = -1
		}
	} else {
		p.exitCode = 0
	}

	// Close the log writers — flushes any buffered output and lets the
	// next start use fresh ones. Supervisor would occasionally leak fds
	// here; we'd rather not.
	if p.stdoutWriter != nil {
		p.stdoutWriter.Close()
	}
	if p.stderrWriter != nil {
		p.stderrWriter.Close()
	}

	// Snapshot the bits we need before unlocking — we're about to call
	// emitStateChange and friends, and we don't want to hold the mutex
	// across a callback.
	wasRunning := p.state == types.StateRunning || p.state == types.StateStarting
	wasStopping := p.state == types.StateStopping
	startedAt := p.startTime
	exitCode := p.exitCode
	config := p.config

	if wasStopping {
		// We asked it to stop and it did. Done. No restart.
		p.state = types.StateStopped
		p.pid = 0
		p.mu.Unlock()
		p.emitStateChange(types.StateStopping, types.StateStopped)
		p.logger.Info("process stopped", "name", config.Name, "exit_code", exitCode)
		return
	}

	// Did it stay up long enough to count as "started successfully"? If
	// not, it's a quick crash and we go to BACKOFF.
	uptime := p.stopTime.Sub(startedAt)
	startedSuccessfully := uptime >= time.Duration(config.StartSecs)*time.Second

	if !startedSuccessfully && wasRunning {
		// Crash-loop territory. Bump the retry count.
		p.retries++
		if p.retries > config.StartRetries {
			// Out of patience. FATAL. The user will need to fix it and
			// kick it manually.
			p.state = types.StateFatal
			p.pid = 0
			p.mu.Unlock()
			p.emitStateChange(types.StateBackoff, types.StateFatal)
			p.logger.Error("process entered FATAL state after max retries",
				"name", config.Name, "retries", p.retries)
			return
		}
		p.state = types.StateBackoff
		p.pid = 0
		p.mu.Unlock()
		p.emitStateChange(types.StateRunning, types.StateBackoff)

		p.logger.Warn("process exited too quickly, backing off",
			"name", config.Name, "retry", p.retries, "max_retries", config.StartRetries)

		// Linear backoff (retries seconds). Not exponential, despite the
		// label the original comment had — keeps the recovery time
		// predictable, which matters more than asymptotic politeness when
		// you're trying to bring something back up. If the supervisor's
		// shutting down mid-backoff, ctx.Done() bails us out.
		backoff := time.Duration(p.retries) * time.Second
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}

		p.spawn()
		return
	}

	// It stayed up. That counts as a successful start; clear the retry
	// counter so a future crash gets a fresh budget.
	p.retries = 0

	// Now decide whether to restart based on the policy.
	shouldRestart := false
	switch config.AutoRestart {
	case types.RestartAlways:
		shouldRestart = true
	case types.RestartOnFailure:
		// Only restart on *unexpected* exits, where "expected" means
		// "exit code is in the configured ExitCodes list".
		shouldRestart = !isExpectedExit(exitCode, config.ExitCodes)
	case types.RestartNever:
		shouldRestart = false
	}

	if shouldRestart {
		p.state = types.StateExited
		p.pid = 0
		p.mu.Unlock()
		p.emitStateChange(types.StateRunning, types.StateExited)

		p.logger.Info("restarting process", "name", config.Name, "exit_code", exitCode, "policy", config.AutoRestart)
		time.Sleep(1 * time.Second) // tiny breather so we don't hammer
		p.spawn()
		return
	}

	// Exited, no restart. End of the line for now — user can manually start it again.
	p.state = types.StateExited
	p.pid = 0
	p.mu.Unlock()
	p.emitStateChange(types.StateRunning, types.StateExited)
	p.logger.Info("process exited", "name", config.Name, "exit_code", exitCode)
}

// terminate sends the configured stop signal then waits, falling back to
// SIGKILL after StopWaitSecs. This is the function that actually fixes
// supervisor's truly shit "stop" behaviour — it would happily declare a
// process stopped while the OS still had it limping around as a zombie,
// because it didn't bother waiting on the child properly. We do.
func (p *Process) terminate() error {
	p.mu.RLock()
	cmd := p.cmd
	cfg := p.config
	p.mu.RUnlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// Polite first.
	if err := sendSignal(cmd.Process, cfg.StopSignal); err != nil {
		p.logger.Warn("failed to send stop signal", "name", cfg.Name, "signal", cfg.StopSignal, "error", err)
	}

	// Wait for monitor() to close p.done, or for the grace period to lapse.
	select {
	case <-p.done:
		return nil
	case <-time.After(time.Duration(cfg.StopWaitSecs) * time.Second):
		// Right, you had your chance. Hard kill.
		p.logger.Warn("process did not stop gracefully, killing", "name", cfg.Name)
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("killing process %s: %w", cfg.Name, err)
		}
		<-p.done
		return nil
	}
}

// Wait blocks until the current incarnation of the process has exited.
// "Current" matters — if we auto-restart, p.done is replaced and you'll
// only see the death of the run you were watching.
func (p *Process) Wait() {
	<-p.done
}

// setupLogWriter resolves the log file path (with the "AUTO" / "NONE"
// magic values supervisor inherited) and returns a writer. NONE/dev-null
// gets a discard writer; AUTO or empty gets a sensible default path.
func (p *Process) setupLogWriter(path string, maxBytes int64, backups int, stream string) (io.WriteCloser, error) {
	if path == "" || path == "AUTO" {
		path = fmt.Sprintf("/var/log/direktor/%s-%s.log", p.config.Name, stream)
	}
	if path == "NONE" || path == "/dev/null" {
		return &nopWriteCloser{io.Discard}, nil
	}

	return logging.NewRotatingWriter(path, maxBytes, backups)
}

// parseCommand splits a command line on whitespace, respecting single and
// double quotes. Not a full shell parser — no escapes, no expansion, no
// pipelines, no environment substitution. If you want shell features, write
// `command = /bin/sh -c "your big shell pipeline"`.
//
// The reason it's deliberately limited: supervisor leaned on shell parsing
// in places and people kept getting surprised by it. Boring is better here.
func parseCommand(cmd string) []string {
	var args []string
	var current strings.Builder
	inQuote := false
	quoteChar := byte(0)

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case !inQuote && (c == '"' || c == '\''):
			inQuote = true
			quoteChar = c
		case inQuote && c == quoteChar:
			inQuote = false
		case !inQuote && c == ' ':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// isExpectedExit — is this exit code in the user's "this is fine" list?
// Used by RestartOnFailure to decide whether the exit was, well, a failure.
func isExpectedExit(code int, expected []int) bool {
	for _, e := range expected {
		if code == e {
			return true
		}
	}
	return false
}

// formatDuration prints uptime the way humans read it. "2d 3h 17m 4s",
// dropping leading zero-units so a fresh process doesn't say "0d 0h 0m 12s"
// like an over-eager intern.
func formatDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// nopWriteCloser is the discard writer for "log to nowhere". io.Discard
// doesn't have a Close method, so we glue one on.
type nopWriteCloser struct {
	io.Writer
}

func (n *nopWriteCloser) Close() error { return nil }

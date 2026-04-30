package process

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/andydixon/direktor/internal/logging"
	"github.com/andydixon/direktor/pkg/types"
)

// TestProcessStartStop is the smoke test — fire up a long-running thing,
// confirm it lands in RUNNING, ask it to stop, confirm it ends up STOPPED.
// Anything more elaborate gets flaky on CI, so we keep it boring.
func TestProcessStartStop(t *testing.T) {
	logger := logging.New("debug", os.Stdout)

	// Pick a "sleep for ages" command that exists on whichever OS we're on.
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "cmd /c ping -n 100 127.0.0.1"
	} else {
		cmd = "sleep 100"
	}

	cfg := types.ProcessConfig{
		Name:          "test-sleep",
		Command:       cmd,
		AutoStart:     false,
		AutoRestart:   types.RestartNever,
		StartSecs:     1,
		StartRetries:  3,
		StopSignal:    "TERM",
		StopWaitSecs:  5,
		ExitCodes:     []int{0},
		StdoutLogFile: "NONE",
		StderrLogFile: "NONE",
	}

	p := New(cfg, logger)

	// Fresh process should be STOPPED, not running, not anything else.
	if p.State() != types.StateStopped {
		t.Fatalf("initial state = %s, want STOPPED", p.State())
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// StartSecs is 1, so 2 seconds is enough to be confidently RUNNING.
	time.Sleep(2 * time.Second)
	if p.State() != types.StateRunning {
		t.Fatalf("state after start = %s, want RUNNING", p.State())
	}

	info := p.Info()
	if info.PID == 0 {
		t.Error("PID should not be 0 when running")
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	p.Wait()
	if p.State() != types.StateStopped {
		t.Fatalf("state after stop = %s, want STOPPED", p.State())
	}
}

// TestProcessAutoRestart — a process that exits immediately with autorestart=always.
// We don't assert a specific final state because timing is genuinely
// unpredictable here; we just want to know nothing panics or wedges.
func TestProcessAutoRestart(t *testing.T) {
	logger := logging.New("debug", os.Stdout)

	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "cmd /c exit 0"
	} else {
		cmd = "sh -c exit 0"
	}

	cfg := types.ProcessConfig{
		Name:          "test-restart",
		Command:       cmd,
		AutoStart:     false,
		AutoRestart:   types.RestartAlways,
		StartSecs:     0,
		StartRetries:  3,
		StopSignal:    "TERM",
		StopWaitSecs:  5,
		ExitCodes:     []int{0},
		StdoutLogFile: "NONE",
		StderrLogFile: "NONE",
	}

	p := New(cfg, logger)

	if err := p.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// With StartSecs=0 the process is "started" the instant it spawns,
	// then it exits, then autorestart=always kicks in. State will be
	// somewhere in the STARTING/RUNNING/BACKOFF range depending on
	// exactly when we look. So we just log it and move on.
	time.Sleep(3 * time.Second)

	state := p.State()
	t.Logf("State after quick exit with autorestart=always: %s", state)

	// Tidy up so we don't leak goroutines into other tests.
	if state == types.StateRunning || state == types.StateStarting || state == types.StateBackoff {
		p.Stop()
	}
}

// TestProcessExitCodes — the boring "is this number in this list" test.
// Mostly here as a guard against someone "simplifying" isExpectedExit and
// breaking the on-failure restart policy.
func TestProcessExitCodes(t *testing.T) {
	tests := []struct {
		code     int
		expected []int
		want     bool
	}{
		{0, []int{0}, true},
		{1, []int{0}, false},
		{2, []int{0, 2}, true},
		{137, []int{0}, false}, // 137 = 128 + 9 = SIGKILL'd, definitely a failure
	}

	for _, tt := range tests {
		got := isExpectedExit(tt.code, tt.expected)
		if got != tt.want {
			t.Errorf("isExpectedExit(%d, %v) = %v, want %v", tt.code, tt.expected, got, tt.want)
		}
	}
}

// TestParseCommand makes sure the little quote-aware splitter doesn't lose
// its mind on the things people actually write. Not exhaustive — there's no
// shell-escape support and there isn't going to be — just the obvious cases.
func TestParseCommand(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"ls -la", []string{"ls", "-la"}},
		{"/usr/bin/app --flag value", []string{"/usr/bin/app", "--flag", "value"}},
		{`echo "hello world"`, []string{"echo", "hello world"}},
		{"cmd 'arg with spaces'", []string{"cmd", "arg with spaces"}},
	}

	for _, tt := range tests {
		got := parseCommand(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseCommand(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseCommand(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

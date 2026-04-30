// Package supervisor is the brain of the daemon. It owns the config, the set
// of *process.Process instances, signal handling, the reload story, and the
// notifier hookup.
//
// Roughly: cmd/direktord wires us into the IPC and HTTP servers, calls Start,
// and then sits on Wait() until we shut down. Everything else (commands from
// the CLI, REST API calls, signals from systemd) eventually lands here.
//
// Naming note: yes, the package and the type are both called "supervisor".
// Yes, that does mean "supervisor.Supervisor" which is silly. The alternative
// names are all worse. We're sticking with it.
package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/andydixon/direktor/internal/config"
	"github.com/andydixon/direktor/internal/logging"
	"github.com/andydixon/direktor/internal/notify"
	"github.com/andydixon/direktor/internal/process"
	"github.com/andydixon/direktor/pkg/types"
)

// Supervisor coordinates everything. There is precisely one of these per
// running daemon. Don't try to spin up two — they'd fight over the PID file,
// the socket, and probably your sanity.
type Supervisor struct {
	mu        sync.RWMutex
	cfg       *types.Config
	cfgPath   string
	processes map[string]*process.Process
	logger    *logging.Logger
	notifier  *notify.Notifier
	ctx       context.Context
	cancel    context.CancelFunc
}

// New parses the config, builds a Supervisor, wires up the notifier and
// creates a Process struct (still STOPPED) for every program in the file.
//
// We do NOT start anything here — that's Start's job. This split lets the
// caller wire IPC and HTTP up first, so the moment processes start spitting
// state changes there's somebody listening.
func New(cfgPath string) (*Supervisor, error) {
	cfg, err := config.Parse(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	logger := logging.New(cfg.Supervisor.LogLevel, os.Stdout)

	ctx, cancel := context.WithCancel(context.Background())

	// Notifier needs the hostname so emails say "[Direktor/myhost] ..." —
	// invaluable when you've got a fleet and the alerts all look the same.
	hostname, _ := os.Hostname()
	emailCfg := buildEmailConfig(cfg.Supervisor.Email)
	notifier := notify.NewNotifier(emailCfg, logger, hostname)

	s := &Supervisor{
		cfg:       cfg,
		cfgPath:   cfgPath,
		processes: make(map[string]*process.Process),
		logger:    logger,
		notifier:  notifier,
		ctx:       ctx,
		cancel:    cancel,
	}

	// Build (but don't start) a Process for each [program:x] block.
	// Hook up the state-change callback so transitions flow into the
	// notifier automatically.
	for name, pcfg := range cfg.Programs {
		p := process.New(pcfg, logger)
		p.SetStateChangeCallback(s.onProcessStateChange)
		s.processes[name] = p
	}

	return s, nil
}

// buildEmailConfig translates the config-shape struct into the notifier's
// own struct. Different package, different struct, same data. We keep the
// notifier package's struct independent so it could, in theory, be reused
// without dragging types.Config along.
func buildEmailConfig(ecfg types.EmailNotifyConfig) notify.EmailConfig {
	var notifyOn []types.ProcessState
	for _, s := range ecfg.NotifyOn {
		notifyOn = append(notifyOn, types.ProcessState(s))
	}
	return notify.EmailConfig{
		Enabled:    ecfg.Enabled,
		SMTPHost:   ecfg.SMTPHost,
		SMTPPort:   ecfg.SMTPPort,
		Username:   ecfg.Username,
		Password:   ecfg.Password,
		From:       ecfg.From,
		Recipients: ecfg.Recipients,
		UseTLS:     ecfg.UseTLS,
		NotifyOn:   notifyOn,
	}
}

// onProcessStateChange is the hook every Process calls when its state moves.
// We just forward the event to the notifier; if email's disabled in config
// the notifier no-ops on its own.
func (s *Supervisor) onProcessStateChange(name string, oldState, newState types.ProcessState, pid int, exitCode int) {
	s.notifier.Notify(notify.StateChangeEvent{
		ProcessName: name,
		OldState:    oldState,
		NewState:    newState,
		PID:         pid,
		ExitCode:    exitCode,
		Timestamp:   timeNow(),
	})
}

// timeNow exists so tests can swap it out for a deterministic clock without
// having to introduce an interface for the sake of one function call. Yes,
// it's a global. No, I don't love it either. But the alternative is uglier.
var timeNow = func() time.Time { return time.Now() }

// Config returns the current parsed configuration. Pointer is shared, so
// callers should treat it as read-only — mutating it would race with
// Reload, and you'd not enjoy debugging the result.
func (s *Supervisor) Config() *types.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Logger exposes the supervisor's logger so other components can log
// against the same configured handler/level. Cheaper than passing it
// around everywhere.
func (s *Supervisor) Logger() *logging.Logger {
	return s.logger
}

// Start writes the PID file, starts every autostart process in priority
// order, and installs the signal handler. Doesn't block — the caller does
// that with Wait(). If you call Start twice you'll get two signal handlers
// and a bad day; don't do that.
func (s *Supervisor) Start() error {
	s.logger.Info("direktor starting", "identifier", s.cfg.Supervisor.Identifier)

	// PID file. Best effort — a failure here is logged but not fatal,
	// because plenty of setups don't want one (containerised, foreground
	// for systemd Type=notify, etc.) and we'd rather warn than die.
	if s.cfg.Supervisor.PidFile != "" {
		if err := os.WriteFile(s.cfg.Supervisor.PidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
			s.logger.Warn("failed to write pid file", "path", s.cfg.Supervisor.PidFile, "error", err)
		}
	}

	s.startAutoStartProcesses()

	// Signals. SIGHUP = reload (the unix tradition); SIGTERM/SIGINT = shutdown.
	// We deliberately don't catch SIGQUIT — if you want a core dump, have one.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	go func() {
		for {
			select {
			case sig := <-sigCh:
				switch sig {
				case syscall.SIGHUP:
					s.logger.Info("received SIGHUP, reloading config")
					if err := s.Reload(); err != nil {
						s.logger.Error("config reload failed", "error", err)
					}
				case syscall.SIGTERM, syscall.SIGINT:
					s.logger.Info("received shutdown signal", "signal", sig)
					s.Shutdown()
					return
				}
			case <-s.ctx.Done():
				return
			}
		}
	}()

	return nil
}

// Wait blocks until Shutdown has fully completed. This is what main() parks on.
func (s *Supervisor) Wait() {
	<-s.ctx.Done()
}

// Shutdown stops everything in parallel, removes the PID file, and cancels
// the context (which unblocks Wait).
//
// Why parallel? Because some processes take their full StopWaitSecs to die,
// and stopping ten of them in series turns shutdown into a coffee break.
// Supervisor used to do this serially. Don't ask me why.
func (s *Supervisor) Shutdown() {
	s.logger.Info("shutting down all processes")

	// Snapshot the process list so we don't hold the lock across the wait.
	s.mu.RLock()
	procs := make([]*process.Process, 0, len(s.processes))
	for _, p := range s.processes {
		procs = append(procs, p)
	}
	s.mu.RUnlock()

	var wg sync.WaitGroup
	for _, p := range procs {
		if p.State() == types.StateRunning || p.State() == types.StateStarting {
			wg.Add(1)
			go func(proc *process.Process) {
				defer wg.Done()
				if err := proc.Stop(); err != nil {
					s.logger.Warn("error stopping process during shutdown", "name", proc.Name(), "error", err)
				}
			}(p)
		}
	}
	wg.Wait()

	// PID file: clean up if we wrote one. Ignoring the error — file might
	// already be gone (sysadmin tidied up, container layer, etc.) and we
	// don't care.
	if s.cfg.Supervisor.PidFile != "" {
		os.Remove(s.cfg.Supervisor.PidFile)
	}

	s.cancel()
	s.logger.Info("direktor shut down complete")
}

// Reload re-reads the config and applies the diff against what's currently
// loaded. New programs get a Process and (if autostart) start; removed
// programs get stopped and dropped; existing programs get their config
// updated in place (takes effect on next start, deliberately).
//
// We do *not* automatically restart programs whose config changed. That's
// supervisor's behaviour and it lost people's nerves on a regular basis.
// Operators get to choose when to bounce a service.
func (s *Supervisor) Reload() error {
	newCfg, err := config.Parse(s.cfgPath)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// New + changed: create or update. UpdateConfig doesn't restart.
	for name, pcfg := range newCfg.Programs {
		if existing, ok := s.processes[name]; ok {
			existing.UpdateConfig(pcfg)
		} else {
			p := process.New(pcfg, s.logger)
			p.SetStateChangeCallback(s.onProcessStateChange)
			s.processes[name] = p
			if pcfg.AutoStart {
				go p.Start()
			}
		}
	}

	// Removed: stop running ones and drop them from the map.
	for name, p := range s.processes {
		if _, ok := newCfg.Programs[name]; !ok {
			if p.State() == types.StateRunning {
				go p.Stop()
			}
			delete(s.processes, name)
		}
	}

	s.cfg = newCfg
	s.logger.Info("configuration reloaded", "programs", len(newCfg.Programs))
	return nil
}

// StartProcess starts a named process. Wraps the lookup so callers don't
// have to think about the map.
func (s *Supervisor) StartProcess(name string) error {
	s.mu.RLock()
	p, ok := s.processes[name]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown process: %s", name)
	}

	return p.Start()
}

// StopProcess — the obvious counterpart to StartProcess.
func (s *Supervisor) StopProcess(name string) error {
	s.mu.RLock()
	p, ok := s.processes[name]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown process: %s", name)
	}

	return p.Stop()
}

// RestartProcess restarts a named process. Process.Restart handles the
// stop-then-start dance; we just look it up.
func (s *Supervisor) RestartProcess(name string) error {
	s.mu.RLock()
	p, ok := s.processes[name]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown process: %s", name)
	}

	return p.Restart()
}

// Status returns a sorted slice of ProcessInfo for every managed process.
// Sorted by name for stable output — nothing worse than `direktorctl status`
// shuffling rows around between calls and making your eyes do extra work.
func (s *Supervisor) Status() []types.ProcessInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	infos := make([]types.ProcessInfo, 0, len(s.processes))
	for _, p := range s.processes {
		infos = append(infos, p.Info())
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Name < infos[j].Name
	})

	return infos
}

// ProcessStatus returns info for one named process, or an error if there's
// no such process.
func (s *Supervisor) ProcessStatus(name string) (*types.ProcessInfo, error) {
	s.mu.RLock()
	p, ok := s.processes[name]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown process: %s", name)
	}

	info := p.Info()
	return &info, nil
}

// AddProcess wires a new process up at runtime. Used by the API's
// POST /api/processes — supervisor wanted you to edit the config and
// reread/update; we let you push directly. Less faff.
//
// Note: this also writes the new program into the in-memory s.cfg.Programs
// map so it survives a Status() call cleanly. It does NOT write back to the
// config file on disk — if you want it persisted, you're on your own.
func (s *Supervisor) AddProcess(cfg types.ProcessConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.processes[cfg.Name]; exists {
		return fmt.Errorf("process %s already exists", cfg.Name)
	}

	p := process.New(cfg, s.logger)
	p.SetStateChangeCallback(s.onProcessStateChange)
	s.processes[cfg.Name] = p

	s.cfg.Programs[cfg.Name] = cfg

	s.logger.Info("process added", "name", cfg.Name)

	if cfg.AutoStart {
		go p.Start()
	}

	return nil
}

// RemoveProcess stops the process (if it's up) and forgets about it. Like
// AddProcess this is in-memory only — disk config is untouched.
func (s *Supervisor) RemoveProcess(name string) error {
	s.mu.Lock()
	p, ok := s.processes[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown process: %s", name)
	}
	delete(s.processes, name)
	delete(s.cfg.Programs, name)
	s.mu.Unlock()

	if p.State() == types.StateRunning || p.State() == types.StateStarting {
		p.Stop()
	}

	s.logger.Info("process removed", "name", name)
	return nil
}

// startAutoStartProcesses boots everything with autostart=true, lowest
// priority value first. The ordering matters when programs depend on each
// other (your DB before your app, your app before your worker) — supervisor
// honoured this and it's one of the things people legitimately rely on, so
// we keep it.
func (s *Supervisor) startAutoStartProcesses() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type namedProc struct {
		name     string
		priority int
		proc     *process.Process
	}

	var procs []namedProc
	for name, p := range s.processes {
		cfg := p.Config()
		if cfg.AutoStart {
			procs = append(procs, namedProc{name: name, priority: cfg.Priority, proc: p})
		}
	}

	// Lower priority number = starts first. Yes, that's backwards from
	// what "priority" suggests in everyday English, but it's the
	// supervisor convention and changing it would break everyone's configs.
	sort.Slice(procs, func(i, j int) bool {
		return procs[i].priority < procs[j].priority
	})

	for _, np := range procs {
		if err := np.proc.Start(); err != nil {
			s.logger.Error("failed to autostart process", "name", np.name, "error", err)
		}
	}
}

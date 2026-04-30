// Package web is the embedded HTTP server — JSON API plus the (single-page,
// no-build-step, deliberately tiny) web UI.
//
// The supervisor's web UI was famously horrible. It was rendered server-side
// via Python's ancient HTML library and looked like 2003. Ours isn't going
// to win design awards either, but at least it's responsive, has dark mode,
// and renders without a network round-trip per click. Small mercies.
//
// The auth story is intentionally minimal — token (Bearer) or basic, and
// that's it. If you need OIDC / SAML / PAM / whatever, run direktor behind
// a real reverse proxy (nginx, caddy, traefik) and let it handle the auth.
// Bolting half a dozen schemes into here would add bugs faster than features.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/andydixon/direktor/internal/logging"
	"github.com/andydixon/direktor/internal/supervisor"
	"github.com/andydixon/direktor/pkg/types"
)

// Server is the HTTP server. One per daemon. Auth bits are cached at
// construction so middleware doesn't re-read config on every request.
type Server struct {
	supervisor *supervisor.Supervisor
	logger     *logging.Logger
	httpServer *http.Server
	authToken  string // shared secret, or basic-auth password
	authType   string // "" (off), "token", or "basic"
}

// NewServer constructs the Server. Doesn't bind anything — call Start.
func NewServer(sup *supervisor.Supervisor, logger *logging.Logger) *Server {
	cfg := sup.Config()
	return &Server{
		supervisor: sup,
		logger:     logger,
		authToken:  cfg.Supervisor.HTTPAuthToken,
		authType:   cfg.Supervisor.HTTPAuth,
	}
}

// Start binds the listener and kicks off the serve goroutine. Returns once
// the listener's up; if binding fails (port in use, permissions) you get
// the error immediately, which is the only sensible time to fail loudly.
func (s *Server) Start() error {
	cfg := s.supervisor.Config()
	addr := fmt.Sprintf("%s:%d", cfg.Supervisor.HTTPHost, cfg.Supervisor.HTTPPort)

	mux := http.NewServeMux()

	// API routes. The 1.22 method-prefix syntax (`POST /foo`) means we
	// don't need a third-party router — a small but real win.
	mux.HandleFunc("GET /api/processes", s.authMiddleware(s.handleListProcesses))
	mux.HandleFunc("GET /api/processes/{name}", s.authMiddleware(s.handleGetProcess))
	mux.HandleFunc("POST /api/processes/{name}/start", s.authMiddleware(s.handleStartProcess))
	mux.HandleFunc("POST /api/processes/{name}/stop", s.authMiddleware(s.handleStopProcess))
	mux.HandleFunc("POST /api/processes/{name}/restart", s.authMiddleware(s.handleRestartProcess))
	mux.HandleFunc("DELETE /api/processes/{name}", s.authMiddleware(s.handleRemoveProcess))
	mux.HandleFunc("POST /api/processes", s.authMiddleware(s.handleAddProcess))
	mux.HandleFunc("GET /api/logs/{name}", s.authMiddleware(s.handleGetLogs))
	mux.HandleFunc("POST /api/reload", s.authMiddleware(s.handleReload))

	// Web UI — the SPA-in-a-string at the bottom of this file. No auth
	// on the page itself; the API calls it makes go through middleware.
	mux.HandleFunc("GET /", s.handleUI)

	// Timeouts: generous enough that slow clients don't get cut off,
	// tight enough that we're not vulnerable to slowloris-style nonsense.
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("binding HTTP server to %s: %w", addr, err)
	}

	s.logger.Info("HTTP server listening", "address", addr)

	go func() {
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully drains in-flight requests and shuts the server. 10s ought
// to be more than enough; anything longer is a bug somewhere.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// authMiddleware wraps a handler with the configured auth scheme. If auth
// is off (no type, no token), it's a no-op pass-through.
//
// Yes, comparing the token with `==` is a timing-side-channel risk in the
// strictest sense. In practice, if you can run a timing attack against your
// own local supervisor's HTTP port, you've already lost — they're inside
// the network. We could subtle.ConstantTimeCompare here; we don't, because
// it'd add complexity for a threat that doesn't apply.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authType == "" || s.authToken == "" {
			next(w, r)
			return
		}

		switch s.authType {
		case "token":
			token := r.Header.Get("Authorization")
			token = strings.TrimPrefix(token, "Bearer ")
			if token != s.authToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		case "basic":
			_, password, ok := r.BasicAuth()
			if !ok || password != s.authToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="Direktor"`)
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}

		next(w, r)
	}
}

// handleListProcesses — GET /api/processes — returns the lot, sorted.
func (s *Server) handleListProcesses(w http.ResponseWriter, r *http.Request) {
	infos := s.supervisor.Status()
	writeJSON(w, http.StatusOK, infos)
}

// handleGetProcess — GET /api/processes/{name} — single process info.
// 404 if it doesn't exist (rather than silently returning an empty object,
// which is what supervisor's API loved to do).
func (s *Server) handleGetProcess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	info, err := s.supervisor.ProcessStatus(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, types.Response{Success: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleStartProcess — POST /api/processes/{name}/start.
func (s *Server) handleStartProcess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.supervisor.StartProcess(name); err != nil {
		writeJSON(w, http.StatusBadRequest, types.Response{Success: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, types.Response{Success: true, Message: fmt.Sprintf("started: %s", name)})
}

// handleStopProcess — POST /api/processes/{name}/stop.
func (s *Server) handleStopProcess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.supervisor.StopProcess(name); err != nil {
		writeJSON(w, http.StatusBadRequest, types.Response{Success: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, types.Response{Success: true, Message: fmt.Sprintf("stopped: %s", name)})
}

// handleRestartProcess — POST /api/processes/{name}/restart.
func (s *Server) handleRestartProcess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.supervisor.RestartProcess(name); err != nil {
		writeJSON(w, http.StatusBadRequest, types.Response{Success: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, types.Response{Success: true, Message: fmt.Sprintf("restarted: %s", name)})
}

// handleGetLogs — GET /api/logs/{name}?stream=stdout|stderr.
//
// Streams the *tail* (last 100KB) as plain text. Not the whole file —
// supervisor's UI happily tried to render multi-gigabyte log files inline
// and would brick browsers doing it. If you want the whole thing, you've
// got `cat` and `less` on the box.
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "stdout"
	}

	info, err := s.supervisor.ProcessStatus(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, types.Response{Success: false, Message: err.Error()})
		return
	}

	logPath := info.LogFile
	if stream == "stderr" {
		logPath = info.StderrLog
	}

	if logPath == "" {
		writeJSON(w, http.StatusNotFound, types.Response{Success: false, Message: "no log file configured"})
		return
	}

	data, err := readTail(logPath, 100*1024)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, types.Response{Success: false, Message: err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(data)
}

// handleReload — POST /api/reload — re-parses the config and applies the diff.
// Same logic as SIGHUP / direktorctl reload.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.supervisor.Reload(); err != nil {
		writeJSON(w, http.StatusInternalServerError, types.Response{Success: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, types.Response{Success: true, Message: "configuration reloaded"})
}

// AddProcessRequest is the JSON body for POST /api/processes — i.e. adding a
// new managed process at runtime without editing the config file.
//
// The pointer-to-bool / pointer-to-int fields are deliberate: a JSON `false`
// looks identical to "not provided" once unmarshalled into a plain bool, and
// we need to distinguish "user explicitly said autostart=false" from "user
// didn't say" so the defaults don't get clobbered with zero values. Yes,
// it's ugly. Yes, it's the standard Go workaround. Sigh.
type AddProcessRequest struct {
	Name          string `json:"name"`
	Command       string `json:"command"`
	Directory     string `json:"directory"`
	User          string `json:"user"`
	AutoStart     *bool  `json:"autostart"`
	AutoRestart   string `json:"autorestart"`
	StartSecs     *int   `json:"startsecs"`
	StartRetries  *int   `json:"startretries"`
	StopSignal    string `json:"stopsignal"`
	StopWaitSecs  *int   `json:"stopwaitsecs"`
	StdoutLogFile string `json:"stdout_logfile"`
	StderrLogFile string `json:"stderr_logfile"`
	Environment   map[string]string `json:"environment"`
	RedirectStderr *bool `json:"redirect_stderr"`
}

// handleAddProcess — POST /api/processes — wires a new managed process up
// at runtime. In-memory only; doesn't touch the config file.
func (s *Server) handleAddProcess(w http.ResponseWriter, r *http.Request) {
	var req AddProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, types.Response{Success: false, Message: "invalid JSON: " + err.Error()})
		return
	}

	// Required-field guards. Everything else has a sensible default.
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, types.Response{Success: false, Message: "name is required"})
		return
	}
	if req.Command == "" {
		writeJSON(w, http.StatusBadRequest, types.Response{Success: false, Message: "command is required"})
		return
	}

	// Start with the same defaults as a freshly-parsed config block, then
	// overlay anything the request actually specified. (Yes, the defaults
	// are duplicated from config.defaultProcessConfig — the alternative was
	// exposing that helper, and right now nothing else needs it.)
	cfg := types.ProcessConfig{
		Name:              req.Name,
		Command:           req.Command,
		Directory:         req.Directory,
		User:              req.User,
		AutoStart:         true,
		AutoRestart:       types.RestartAlways,
		StartSecs:         1,
		StartRetries:      3,
		StopSignal:        "TERM",
		StopWaitSecs:      10,
		ExitCodes:         []int{0},
		Priority:          999,
		NumProcs:          1,
		StdoutLogFile:     req.StdoutLogFile,
		StderrLogFile:     req.StderrLogFile,
		StdoutLogMaxBytes: 50 * 1024 * 1024,
		StderrLogMaxBytes: 50 * 1024 * 1024,
		StdoutLogBackups:  10,
		StderrLogBackups:  10,
		Environment:       req.Environment,
	}

	if cfg.Environment == nil {
		cfg.Environment = make(map[string]string)
	}

	if req.AutoStart != nil {
		cfg.AutoStart = *req.AutoStart
	}
	if req.AutoRestart != "" {
		switch req.AutoRestart {
		case "always":
			cfg.AutoRestart = types.RestartAlways
		case "on-failure":
			cfg.AutoRestart = types.RestartOnFailure
		case "never":
			cfg.AutoRestart = types.RestartNever
		}
	}
	if req.StartSecs != nil {
		cfg.StartSecs = *req.StartSecs
	}
	if req.StartRetries != nil {
		cfg.StartRetries = *req.StartRetries
	}
	if req.StopSignal != "" {
		cfg.StopSignal = req.StopSignal
	}
	if req.StopWaitSecs != nil {
		cfg.StopWaitSecs = *req.StopWaitSecs
	}
	if req.RedirectStderr != nil {
		cfg.RedirectStderr = *req.RedirectStderr
	}

	if err := s.supervisor.AddProcess(cfg); err != nil {
		writeJSON(w, http.StatusConflict, types.Response{Success: false, Message: err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, types.Response{Success: true, Message: fmt.Sprintf("process %s added", req.Name)})
}

// handleRemoveProcess — DELETE /api/processes/{name} — stops it (if up) and
// removes it from the in-memory program list.
func (s *Server) handleRemoveProcess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.supervisor.RemoveProcess(name); err != nil {
		writeJSON(w, http.StatusNotFound, types.Response{Success: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, types.Response{Success: true, Message: fmt.Sprintf("process %s removed", name)})
}

// handleUI just slings the embedded HTML at the browser. The whole UI is
// one big string constant at the bottom of the file — no asset pipeline,
// no build step, no node_modules, no nightmares.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(webUIHTML))
}

// writeJSON is the boring helper. Sets the content-type, writes the status,
// encodes the body. Nothing surprising.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// readTail returns the last `maxBytes` of a file. If the file's smaller than
// that we just hand back the whole thing.
//
// Note: on the cut-off case we read exactly maxBytes from `size-maxBytes`,
// which means the first line is almost certainly half-eaten. The browser
// renders it as garbled text on the very first row of the log viewer; in
// practice nobody minds because logs are line-after-line and you don't read
// the top one anyway. If this ever becomes a problem we can scan forward to
// the next newline; for now it's fine.
func readTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	size := info.Size()
	if size <= maxBytes {
		return os.ReadFile(path)
	}

	buf := make([]byte, maxBytes)
	_, err = f.ReadAt(buf, size-maxBytes)
	return buf, err
}

// webUIHTML is the entire web UI as one big template-free HTML string.
//
// Yes, it's enormous. Yes, it would be tidier to use html/template and
// embed.FS. No, that's not going to happen — keeping the UI in one self-
// contained string means there's no asset-pipeline question, no template-
// vs-static debate, no embed pitfalls, and `go build` produces a single
// binary that just works. The CSS is hand-rolled rather than tailwind/etc.
// for the same reason.
//
// If you find yourself adding more than ~200 lines of JS in here, that's
// the signal it's time to break this out into a proper SPA in another
// directory. We're not there yet.
const webUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Direktor — Process Supervisor</title>
    <style>
        /* Material Design — charcoal dark theme.
           Surfaces follow Material's elevation tokens (12dp/24dp tints on #121212),
           accent is Material Teal A400, status colours map to RUNNING/BACKOFF/EXITED. */
        :root {
            --bg:           #121212;            /* Material dark surface 0 */
            --surface-1:    #1E1E1E;            /* 1dp elevation */
            --surface-2:    #232323;            /* 2dp elevation */
            --surface-3:    #2C2C2C;            /* 6dp elevation (table header, hover) */
            --surface-4:    #353535;            /* 12dp (modal, popover) */
            --divider:      rgba(255,255,255,0.08);
            --border:       rgba(255,255,255,0.12);
            --text:         rgba(255,255,255,0.87);
            --text-muted:   rgba(255,255,255,0.60);
            --text-faint:   rgba(255,255,255,0.38);

            --accent:       #1DE9B6;            /* Teal A400 */
            --accent-ink:   #003D33;            /* readable dark on accent */
            --green:        #4CAF50;            /* Green 500 */
            --green-soft:   #69F0AE;            /* Green A200 */
            --amber:        #FFC107;            /* Amber 500 */
            --amber-soft:   #FFD54F;            /* Amber 300 */
            --red:          #F44336;            /* Red 500 */
            --red-soft:     #FF8A80;            /* Red A100 */
            --grey:         #9E9E9E;            /* Grey 500 */
            --grey-soft:    #BDBDBD;            /* Grey 400 */
        }

        * { margin: 0; padding: 0; box-sizing: border-box; }
        html, body { background: var(--bg); }
        body {
            font-family: 'Roboto', -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
            color: var(--text);
            padding: 32px 20px;
            -webkit-font-smoothing: antialiased;
            min-height: 100vh;
        }
        .container { max-width: 1200px; margin: 0 auto; }

        /* Header bar */
        .header {
            display: flex; justify-content: space-between; align-items: center;
            margin-bottom: 24px;
            padding-bottom: 16px;
            border-bottom: 1px solid var(--divider);
        }
        .brand { display: flex; align-items: center; gap: 12px; }
        .brand-mark {
            width: 36px; height: 36px; border-radius: 8px;
            background: linear-gradient(135deg, var(--accent) 0%, #00BFA5 100%);
            display: grid; place-items: center;
            color: var(--accent-ink); font-weight: 700; font-size: 18px;
            box-shadow: 0 4px 12px rgba(29,233,182,0.20);
        }
        h1 {
            font-size: 22px; font-weight: 500; letter-spacing: 0.2px;
            color: var(--text);
        }
        h1 .sub { color: var(--text-muted); font-weight: 400; font-size: 14px; margin-left: 8px; }
        h2 { color: var(--text); margin-bottom: 16px; font-size: 18px; font-weight: 500; }
        .header-buttons { display: flex; gap: 8px; }

        /* Card / table */
        .card {
            background: var(--surface-1);
            border-radius: 12px;
            border: 1px solid var(--divider);
            box-shadow: 0 1px 2px rgba(0,0,0,0.3), 0 4px 16px rgba(0,0,0,0.2);
            overflow: hidden;
        }
        table { width: 100%; border-collapse: collapse; }
        th, td { padding: 14px 18px; text-align: left; }
        thead tr {
            background: var(--surface-3);
            border-bottom: 1px solid var(--border);
        }
        th {
            color: var(--text-muted);
            font-weight: 500;
            font-size: 11px;
            letter-spacing: 0.8px;
            text-transform: uppercase;
        }
        tbody tr { border-bottom: 1px solid var(--divider); transition: background 120ms ease; }
        tbody tr:last-child { border-bottom: none; }
        tbody tr:hover { background: var(--surface-2); }
        td.name { font-weight: 500; color: var(--text); }
        td.dim { color: var(--text-muted); font-variant-numeric: tabular-nums; }

        /* Status chips (Material chip pattern) */
        .status {
            display: inline-flex; align-items: center; gap: 8px;
            padding: 4px 10px 4px 8px;
            border-radius: 999px;
            font-size: 12px;
            font-weight: 500;
            letter-spacing: 0.4px;
            border: 1px solid transparent;
        }
        .status .dot {
            width: 8px; height: 8px; border-radius: 50%;
            background: currentColor;
        }
        .status-RUNNING  { color: var(--green-soft); background: rgba(76,175,80,0.14);  border-color: rgba(76,175,80,0.32); }
        .status-RUNNING .dot { animation: pulse 2s ease-in-out infinite; }
        .status-STARTING { color: var(--amber-soft); background: rgba(255,193,7,0.14);  border-color: rgba(255,193,7,0.32); }
        .status-BACKOFF  { color: var(--amber-soft); background: rgba(255,193,7,0.14);  border-color: rgba(255,193,7,0.32); }
        .status-STOPPING { color: var(--amber-soft); background: rgba(255,193,7,0.14);  border-color: rgba(255,193,7,0.32); }
        .status-EXITED   { color: var(--red-soft);   background: rgba(244,67,54,0.14);  border-color: rgba(244,67,54,0.32); }
        .status-FATAL    { color: var(--red-soft);   background: rgba(244,67,54,0.14);  border-color: rgba(244,67,54,0.32); }
        .status-STOPPED  { color: var(--grey-soft);  background: rgba(158,158,158,0.14); border-color: rgba(158,158,158,0.32); }
        .status-UNKNOWN  { color: var(--grey-soft);  background: rgba(158,158,158,0.14); border-color: rgba(158,158,158,0.32); }

        @keyframes pulse {
            0%, 100% { opacity: 1; transform: scale(1); }
            50%      { opacity: 0.55; transform: scale(0.85); }
        }

        /* Buttons (Material flat / contained) */
        .btn {
            padding: 7px 14px;
            border: none; border-radius: 6px;
            cursor: pointer;
            font-size: 12px; font-weight: 500;
            letter-spacing: 0.3px; text-transform: uppercase;
            margin: 0 2px;
            font-family: inherit;
            transition: background 120ms ease, box-shadow 120ms ease, transform 60ms ease;
        }
        .btn:hover:not(:disabled) { box-shadow: 0 2px 6px rgba(0,0,0,0.4); }
        .btn:active:not(:disabled) { transform: translateY(1px); }
        .btn:disabled { opacity: 0.32; cursor: not-allowed; }

        .btn-start   { background: var(--green); color: #0E2E10; }
        .btn-start:hover:not(:disabled)   { background: #66BB6A; }
        .btn-stop    { background: var(--red); color: #fff; }
        .btn-stop:hover:not(:disabled)    { background: #EF5350; }
        .btn-restart { background: #FFA000; color: #2E1D00; }
        .btn-restart:hover:not(:disabled) { background: #FFB300; }
        .btn-logs    { background: transparent; color: var(--text-muted); border: 1px solid var(--border); }
        .btn-logs:hover:not(:disabled)    { background: var(--surface-3); color: var(--text); }
        .btn-remove  { background: transparent; color: var(--text-faint); padding: 7px 10px; }
        .btn-remove:hover:not(:disabled)  { color: var(--red-soft); background: rgba(244,67,54,0.10); }

        .btn-add {
            background: var(--accent); color: var(--accent-ink);
            font-size: 13px; padding: 10px 18px;
            box-shadow: 0 2px 6px rgba(29,233,182,0.25);
        }
        .btn-add:hover:not(:disabled) { background: #00E5BF; box-shadow: 0 4px 12px rgba(29,233,182,0.35); }
        .btn-refresh {
            background: transparent; color: var(--accent);
            border: 1px solid rgba(29,233,182,0.45);
            font-size: 13px; padding: 9px 16px;
        }
        .btn-refresh:hover:not(:disabled) { background: rgba(29,233,182,0.08); }

        /* Empty state */
        .empty {
            text-align: center; color: var(--text-muted); padding: 48px 20px;
            font-size: 14px;
        }
        .empty b { color: var(--text); }

        /* Logs panel */
        #logs {
            background: #0E0E0E;
            color: #C8C8C8;
            padding: 16px 20px;
            border-radius: 12px;
            border: 1px solid var(--divider);
            margin-top: 24px;
            font-family: 'JetBrains Mono', 'SF Mono', Menlo, Monaco, Consolas, monospace;
            font-size: 12.5px; line-height: 1.55;
            max-height: 420px; overflow-y: auto;
            white-space: pre-wrap;
            display: none;
        }

        /* Modal */
        .modal-overlay {
            display: none; position: fixed; inset: 0;
            background: rgba(0,0,0,0.62);
            backdrop-filter: blur(2px);
            z-index: 1000;
            justify-content: center; align-items: center;
        }
        .modal-overlay.active { display: flex; }
        .modal {
            background: var(--surface-4);
            border-radius: 14px;
            padding: 28px 32px;
            width: 100%; max-width: 620px;
            max-height: 90vh; overflow-y: auto;
            border: 1px solid var(--divider);
            box-shadow: 0 24px 64px rgba(0,0,0,0.55);
        }
        .modal h2 { margin-bottom: 22px; }
        .form-group { margin-bottom: 18px; }
        .form-group label {
            display: block; margin-bottom: 6px;
            font-size: 12px; color: var(--text-muted);
            font-weight: 500; letter-spacing: 0.3px;
            text-transform: uppercase;
        }
        .form-group input, .form-group select, .form-group textarea {
            width: 100%; padding: 11px 13px;
            border: 1px solid var(--border);
            border-radius: 8px;
            background: var(--surface-1);
            color: var(--text);
            font-size: 14px; font-family: inherit;
            transition: border-color 120ms ease, background 120ms ease;
        }
        .form-group input:focus, .form-group select:focus, .form-group textarea:focus {
            outline: none; border-color: var(--accent);
            background: #181818;
        }
        .form-group textarea {
            resize: vertical; min-height: 70px;
            font-family: 'JetBrains Mono', 'SF Mono', Menlo, monospace; font-size: 13px;
        }
        .form-group .hint { font-size: 11px; color: var(--text-faint); margin-top: 6px; }
        .form-row { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
        .form-actions {
            display: flex; justify-content: flex-end; gap: 12px;
            margin-top: 24px; padding-top: 18px;
            border-top: 1px solid var(--divider);
        }
        .form-actions .btn { font-size: 13px; padding: 10px 20px; }
        .btn-cancel { background: transparent; color: var(--text-muted); }
        .btn-cancel:hover:not(:disabled) { background: var(--surface-3); color: var(--text); }
        .required { color: var(--red-soft); }

        /* Toasts */
        .toast {
            position: fixed; bottom: 24px; right: 24px;
            padding: 14px 20px;
            border-radius: 10px;
            font-size: 13.5px; font-weight: 500;
            z-index: 2000;
            box-shadow: 0 8px 24px rgba(0,0,0,0.5);
            animation: slideIn 220ms cubic-bezier(0.4,0,0.2,1);
        }
        .toast-success { background: var(--green); color: #0E2E10; }
        .toast-error   { background: var(--red);   color: #fff; }
        @keyframes slideIn { from { transform: translateY(16px); opacity: 0; } to { transform: translateY(0); opacity: 1; } }

        /* Scrollbars (charcoal) */
        ::-webkit-scrollbar { width: 10px; height: 10px; }
        ::-webkit-scrollbar-track { background: var(--bg); }
        ::-webkit-scrollbar-thumb { background: #3A3A3A; border-radius: 6px; border: 2px solid var(--bg); }
        ::-webkit-scrollbar-thumb:hover { background: #4A4A4A; }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <div class="brand">
                <div class="brand-mark">D</div>
                <h1>Direktor <span class="sub">Process Supervisor</span></h1>
            </div>
            <div class="header-buttons">
                <button class="btn btn-refresh" onclick="refresh()">↻ Refresh</button>
                <button class="btn btn-add" onclick="showAddForm()">+ Add Service</button>
            </div>
        </div>
        <div class="card">
            <table>
                <thead>
                    <tr><th>Name</th><th>Status</th><th>PID</th><th>Uptime</th><th style="text-align:right">Actions</th></tr>
                </thead>
                <tbody id="processes"></tbody>
            </table>
        </div>
        <div id="logs"></div>
    </div>

    <!-- Add Service Modal -->
    <div class="modal-overlay" id="addModal">
        <div class="modal">
            <h2>Add New Service</h2>
            <form id="addForm" onsubmit="submitAddForm(event)">
                <div class="form-group">
                    <label>Name <span class="required">*</span></label>
                    <input type="text" id="f-name" required placeholder="my-service" pattern="[a-zA-Z0-9_-]+" title="Letters, numbers, hyphens, and underscores only">
                    <div class="hint">Unique identifier for this service (letters, numbers, hyphens, underscores)</div>
                </div>
                <div class="form-group">
                    <label>Command <span class="required">*</span></label>
                    <input type="text" id="f-command" required placeholder="/usr/bin/myapp --config /etc/myapp.conf">
                    <div class="hint">Full command to execute including arguments</div>
                </div>
                <div class="form-group">
                    <label>Working Directory</label>
                    <input type="text" id="f-directory" placeholder="/opt/myapp">
                </div>
                <div class="form-row">
                    <div class="form-group">
                        <label>Auto-Start</label>
                        <select id="f-autostart">
                            <option value="true" selected>Yes — start when Direktor starts</option>
                            <option value="false">No — manual start only</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label>Auto-Restart Policy</label>
                        <select id="f-autorestart">
                            <option value="always" selected>Always</option>
                            <option value="on-failure">On Failure (unexpected exit)</option>
                            <option value="never">Never</option>
                        </select>
                    </div>
                </div>
                <div class="form-row">
                    <div class="form-group">
                        <label>Start Seconds</label>
                        <input type="number" id="f-startsecs" value="1" min="0">
                        <div class="hint">Seconds the process must stay up to be considered started</div>
                    </div>
                    <div class="form-group">
                        <label>Start Retries</label>
                        <input type="number" id="f-startretries" value="3" min="0">
                        <div class="hint">Max consecutive restart attempts before FATAL</div>
                    </div>
                </div>
                <div class="form-row">
                    <div class="form-group">
                        <label>Stop Signal</label>
                        <select id="f-stopsignal">
                            <option value="TERM" selected>TERM</option>
                            <option value="HUP">HUP</option>
                            <option value="INT">INT</option>
                            <option value="QUIT">QUIT</option>
                            <option value="KILL">KILL</option>
                            <option value="USR1">USR1</option>
                            <option value="USR2">USR2</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label>Stop Wait (seconds)</label>
                        <input type="number" id="f-stopwaitsecs" value="10" min="1">
                        <div class="hint">Grace period before force-killing</div>
                    </div>
                </div>
                <div class="form-row">
                    <div class="form-group">
                        <label>Stdout Log File</label>
                        <input type="text" id="f-stdout-log" placeholder="AUTO (or full path)">
                    </div>
                    <div class="form-group">
                        <label>Stderr Log File</label>
                        <input type="text" id="f-stderr-log" placeholder="AUTO (or full path)">
                    </div>
                </div>
                <div class="form-group">
                    <label>Environment Variables</label>
                    <textarea id="f-env" placeholder='KEY=value&#10;ANOTHER_KEY=another value'></textarea>
                    <div class="hint">One per line: KEY=value</div>
                </div>
                <div class="form-group">
                    <label>User</label>
                    <input type="text" id="f-user" placeholder="(optional) run as this user">
                </div>
                <div class="form-actions">
                    <button type="button" class="btn btn-cancel" onclick="hideAddForm()">Cancel</button>
                    <button type="submit" class="btn btn-add">Add Service</button>
                </div>
            </form>
        </div>
    </div>

    <script>
        async function refresh() {
            try {
                const resp = await fetch('/api/processes');
                const procs = await resp.json();
                const tbody = document.getElementById('processes');
                if (!procs || procs.length === 0) {
                    tbody.innerHTML = '<tr><td colspan="5" class="empty">No services configured. Click <b>+ Add Service</b> to get started.</td></tr>';
                    return;
                }
                tbody.innerHTML = procs.map(p => ` + "`" + `
                    <tr>
                        <td class="name">${p.name}</td>
                        <td><span class="status status-${p.state}"><span class="dot"></span>${p.state}</span></td>
                        <td class="dim">${p.pid || '—'}</td>
                        <td class="dim">${p.uptime || '—'}</td>
                        <td style="text-align:right;white-space:nowrap">
                            <button class="btn btn-start" onclick="action('${p.name}','start')" ${p.state==='RUNNING'?'disabled':''}>Start</button>
                            <button class="btn btn-stop" onclick="action('${p.name}','stop')" ${p.state!=='RUNNING'&&p.state!=='STARTING'?'disabled':''}>Stop</button>
                            <button class="btn btn-restart" onclick="action('${p.name}','restart')">Restart</button>
                            <button class="btn btn-logs" onclick="viewLogs('${p.name}')">Logs</button>
                            <button class="btn btn-remove" onclick="removeProcess('${p.name}')" title="Remove">✕</button>
                        </td>
                    </tr>
                ` + "`" + `).join('');
            } catch(e) {
                console.error('refresh failed:', e);
            }
        }

        async function action(name, act) {
            await fetch(` + "`" + `/api/processes/${name}/${act}` + "`" + `, {method:'POST'});
            setTimeout(refresh, 500);
        }

        async function removeProcess(name) {
            if (!confirm(` + "`" + `Remove service "${name}"? It will be stopped if running.` + "`" + `)) return;
            const resp = await fetch(` + "`" + `/api/processes/${name}` + "`" + `, {method:'DELETE'});
            const data = await resp.json();
            if (data.success) {
                toast('Service removed: ' + name, 'success');
            } else {
                toast(data.message, 'error');
            }
            setTimeout(refresh, 300);
        }

        async function viewLogs(name) {
            const el = document.getElementById('logs');
            const resp = await fetch(` + "`" + `/api/logs/${name}?stream=stdout` + "`" + `);
            el.textContent = await resp.text();
            el.style.display = 'block';
            el.scrollTop = el.scrollHeight;
        }

        function showAddForm() {
            document.getElementById('addModal').classList.add('active');
            document.getElementById('f-name').focus();
        }

        function hideAddForm() {
            document.getElementById('addModal').classList.remove('active');
            document.getElementById('addForm').reset();
        }

        async function submitAddForm(e) {
            e.preventDefault();

            const env = {};
            const envText = document.getElementById('f-env').value.trim();
            if (envText) {
                envText.split('\n').forEach(line => {
                    const idx = line.indexOf('=');
                    if (idx > 0) {
                        env[line.substring(0, idx).trim()] = line.substring(idx+1).trim();
                    }
                });
            }

            const body = {
                name: document.getElementById('f-name').value.trim(),
                command: document.getElementById('f-command').value.trim(),
                directory: document.getElementById('f-directory').value.trim() || undefined,
                user: document.getElementById('f-user').value.trim() || undefined,
                autostart: document.getElementById('f-autostart').value === 'true',
                autorestart: document.getElementById('f-autorestart').value,
                startsecs: parseInt(document.getElementById('f-startsecs').value) || 1,
                startretries: parseInt(document.getElementById('f-startretries').value) || 3,
                stopsignal: document.getElementById('f-stopsignal').value,
                stopwaitsecs: parseInt(document.getElementById('f-stopwaitsecs').value) || 10,
                stdout_logfile: document.getElementById('f-stdout-log').value.trim() || undefined,
                stderr_logfile: document.getElementById('f-stderr-log').value.trim() || undefined,
                environment: Object.keys(env).length > 0 ? env : undefined,
            };

            try {
                const resp = await fetch('/api/processes', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify(body)
                });
                const data = await resp.json();
                if (data.success) {
                    toast('Service added: ' + body.name, 'success');
                    hideAddForm();
                    setTimeout(refresh, 500);
                } else {
                    toast(data.message, 'error');
                }
            } catch(err) {
                toast('Failed to add service: ' + err.message, 'error');
            }
        }

        function toast(msg, type) {
            const el = document.createElement('div');
            el.className = 'toast toast-' + type;
            el.textContent = msg;
            document.body.appendChild(el);
            setTimeout(() => el.remove(), 4000);
        }

        // Close modal on outside click
        document.getElementById('addModal').addEventListener('click', function(e) {
            if (e.target === this) hideAddForm();
        });

        // Close modal on Escape
        document.addEventListener('keydown', function(e) {
            if (e.key === 'Escape') hideAddForm();
        });

        refresh();
        setInterval(refresh, 5000);
    </script>
</body>
</html>`

// Package ipc is the control-channel server. direktorctl talks to direktord
// over this — one JSON command in, one JSON response out, connection closed.
//
// On Unix it's a local socket (/var/run/direktor.sock by default). On
// Windows we currently fall back to loopback TCP because Go's named-pipe
// support is faff and a half — see ipc_windows.go for the apologies.
//
// Why a custom protocol instead of HTTP? Because the HTTP server is a
// separate, optional thing — sometimes you want to control direktor without
// having opened a network port at all. Local sockets are also enforceable
// via filesystem permissions, which is nicer than baking auth into every
// CLI invocation. Supervisor's XML-RPC was, frankly, a mess; this is
// deliberately simpler.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/andydixon/direktor/internal/logging"
	"github.com/andydixon/direktor/internal/supervisor"
	"github.com/andydixon/direktor/pkg/types"
)

// Server is the listener + accept loop for control connections.
type Server struct {
	supervisor *supervisor.Supervisor
	logger     *logging.Logger
	listener   net.Listener
	socketPath string
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewServer constructs a Server. Call Start to actually open the socket.
func NewServer(sup *supervisor.Supervisor, socketPath string, logger *logging.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		supervisor: sup,
		logger:     logger,
		socketPath: socketPath,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start opens the listener (whichever flavour, see ipc_unix/ipc_windows) and
// spawns the accept loop in a goroutine. Returns once the listener is up;
// won't block.
func (s *Server) Start() error {
	listener, err := createListener(s.socketPath)
	if err != nil {
		return fmt.Errorf("creating IPC listener: %w", err)
	}
	s.listener = listener

	s.logger.Info("IPC server listening", "path", s.socketPath)

	go s.acceptLoop()
	return nil
}

// Stop closes the listener and tidies up the socket file (on Unix). Safe to
// call multiple times — the cancel is idempotent and net.Listener.Close
// returns an error on the second go which we cheerfully ignore.
func (s *Server) Stop() {
	s.cancel()
	if s.listener != nil {
		s.listener.Close()
	}
	cleanupListener(s.socketPath)
}

// acceptLoop is the boring "accept forever" goroutine. The select-on-Done is
// what tells us whether an Accept error is "shutting down" (fine) or "the
// world is broken" (log and try again).
func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				s.logger.Error("IPC accept error", "error", err)
				continue
			}
		}
		go s.handleConnection(conn)
	}
}

// handleConnection reads one Command, writes one Response, closes. Wire
// protocol = JSON, framed by the connection lifetime. If the decode fails
// we send back a structured error rather than just hanging up — the CLI
// surfaces it as "Error: invalid command: ...".
func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var cmd types.Command
	if err := decoder.Decode(&cmd); err != nil {
		encoder.Encode(types.Response{Success: false, Message: "invalid command: " + err.Error()})
		return
	}

	resp := s.executeCommand(cmd)
	encoder.Encode(resp)
}

// executeCommand is the dispatch table. Action strings are lowercased so
// `START` and `start` are equivalent — yes, supervisor was case-sensitive
// here and it tripped people up. We're not.
func (s *Server) executeCommand(cmd types.Command) types.Response {
	switch strings.ToLower(cmd.Action) {
	case "status":
		// Single-name query if Args has anything, otherwise the lot.
		if len(cmd.Args) > 0 {
			info, err := s.supervisor.ProcessStatus(cmd.Args[0])
			if err != nil {
				return types.Response{Success: false, Message: err.Error()}
			}
			return types.Response{Success: true, Data: info}
		}
		return types.Response{Success: true, Data: s.supervisor.Status()}

	case "start":
		if len(cmd.Args) == 0 {
			return types.Response{Success: false, Message: "process name required"}
		}
		// All-or-nothing reporting: first failure stops the loop and
		// returns. The processes that already started before that point
		// stay started — partial success is still success for them.
		for _, name := range cmd.Args {
			if err := s.supervisor.StartProcess(name); err != nil {
				return types.Response{Success: false, Message: err.Error()}
			}
		}
		return types.Response{Success: true, Message: fmt.Sprintf("started: %s", strings.Join(cmd.Args, ", "))}

	case "stop":
		if len(cmd.Args) == 0 {
			return types.Response{Success: false, Message: "process name required"}
		}
		for _, name := range cmd.Args {
			if err := s.supervisor.StopProcess(name); err != nil {
				return types.Response{Success: false, Message: err.Error()}
			}
		}
		return types.Response{Success: true, Message: fmt.Sprintf("stopped: %s", strings.Join(cmd.Args, ", "))}

	case "restart":
		if len(cmd.Args) == 0 {
			return types.Response{Success: false, Message: "process name required"}
		}
		for _, name := range cmd.Args {
			if err := s.supervisor.RestartProcess(name); err != nil {
				return types.Response{Success: false, Message: err.Error()}
			}
		}
		return types.Response{Success: true, Message: fmt.Sprintf("restarted: %s", strings.Join(cmd.Args, ", "))}

	case "reread", "reload", "update":
		// Three names for the same operation. supervisor distinguished
		// reread (just re-parse) from update (re-parse and apply); we
		// always do both because anything else just confuses people.
		if err := s.supervisor.Reload(); err != nil {
			return types.Response{Success: false, Message: err.Error()}
		}
		return types.Response{Success: true, Message: "configuration reloaded"}

	case "shutdown":
		// Fire and forget — Shutdown blocks until everything's down,
		// and we want to get the response back to the client first
		// before the daemon disappears under us.
		go s.supervisor.Shutdown()
		return types.Response{Success: true, Message: "shutdown initiated"}

	default:
		return types.Response{Success: false, Message: fmt.Sprintf("unknown action: %s", cmd.Action)}
	}
}

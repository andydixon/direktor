// direktorctl — the little command-line client. It exists so you don't have
// to curl the HTTP API or write a Python script every time you want to
// restart a process at 3am. Talks to direktord over the unix socket (or a
// loopback TCP fallback on Windows, see internal/ipc).
//
// Modelled, roughly, on supervisorctl — same verbs (status, start, stop,
// restart, reread, update, shutdown) so muscle memory doesn't have to be
// retrained. The behaviour underneath is, hopefully, less surprising.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/andydixon/direktor/internal/ipc"
	"github.com/andydixon/direktor/pkg/types"
)

// Where the socket lives by default. Override with $DIREKTOR_SOCKET when
// you're running multiple daemons on one box (which, fair enough, you
// probably shouldn't but I'm not your dad).
const defaultSocketPath = "/var/run/direktor.sock"

// Stamped at build time via -ldflags. Same dance as direktord.
var version = "v0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// Socket path: env var wins, otherwise the default. Most people will
	// never need to touch this.
	socketPath := os.Getenv("DIREKTOR_SOCKET")
	if socketPath == "" {
		socketPath = defaultSocketPath
	}

	command := os.Args[1]
	args := os.Args[2:]

	// Big switch — supervisor used to alias half a dozen commands to mean
	// almost-but-not-quite the same thing (reread vs update, anyone?). We
	// keep the aliases working but route them all to "reload" because, in
	// practice, that's what they all do.
	switch command {
	case "status":
		doStatus(socketPath, args)
	case "start":
		doAction(socketPath, "start", args)
	case "stop":
		doAction(socketPath, "stop", args)
	case "restart":
		doAction(socketPath, "restart", args)
	case "reread", "update", "reload":
		doAction(socketPath, "reload", nil)
	case "shutdown":
		doAction(socketPath, "shutdown", nil)
	case "help", "--help", "-h":
		printUsage()
	case "version", "--version":
		fmt.Println("direktorctl", versionString())
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`direktorctl - Direktor process supervisor control tool

Usage:
  direktorctl <command> [arguments]

Commands:
  status [name]     Show process status (all or specific)
  start <name>      Start a process
  stop <name>       Stop a process
  restart <name>    Restart a process
  reread            Re-read configuration files
  update            Reload configuration and apply changes
  shutdown          Shut down the supervisor

Environment:
  DIREKTOR_SOCKET   Path to control socket (default: /var/run/direktor.sock)
  DIREKTOR_COLOR    Force colour output: always, never, auto (default: auto)
  NO_COLOR          If set (to any value), disables colour output

Status colours (auto when stdout is a TTY):
  green   RUNNING
  amber   STARTING, BACKOFF, STOPPING
  red     EXITED, FATAL
  grey    STOPPED, UNKNOWN`)
}

// doStatus is the slightly fiddly one — the daemon returns either a single
// ProcessInfo (when a name was given) or a slice of them (when not). We try
// both shapes rather than making the wire protocol any cleverer than it
// already is.
func doStatus(socketPath string, args []string) {
	cmd := types.Command{Action: "status", Args: args}
	resp := sendCommand(socketPath, cmd)

	if !resp.Success {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Message)
		os.Exit(1)
	}

	// Re-marshal so we can confidently re-unmarshal into the right shape.
	// A bit wasteful, sure, but it sidesteps interface{} gymnastics.
	data, err := json.Marshal(resp.Data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// If the user asked for one process, optimistically try the single shape first.
	if len(args) > 0 {
		var info types.ProcessInfo
		if err := json.Unmarshal(data, &info); err == nil {
			printProcessInfo([]types.ProcessInfo{info})
			return
		}
	}

	// Otherwise — or as a fallback — treat it as a list.
	var infos []types.ProcessInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		os.Exit(1)
	}

	if len(infos) == 0 {
		fmt.Println("No processes configured.")
		return
	}

	printProcessInfo(infos)
}

// printProcessInfo formats the status table.
//
// Why not text/tabwriter? Because tabwriter counts ANSI escape bytes as part
// of cell width, so once you start colourising the STATUS column the entire
// table tears in half. We do the column maths ourselves — it's not pretty
// but the output stays aligned, which is the whole point of a table.
func printProcessInfo(infos []types.ProcessInfo) {
	colour := useColour()

	const (
		hName    = "NAME"
		hStatus  = "STATUS"
		hPID     = "PID"
		hUptime  = "UPTIME"
		hDescrip = "DESCRIPTION"
	)
	nameW, statusW, pidW, uptimeW := len(hName), len(hStatus), len(hPID), len(hUptime)

	// First pass: work out the widest value in each column, while building
	// the row tuples we'll print on the second pass. Two passes because
	// alignment can't know widths until everything's measured.
	rows := make([][5]string, 0, len(infos))
	for _, info := range infos {
		pid := "-"
		if info.PID > 0 {
			pid = fmt.Sprintf("%d", info.PID)
		}
		uptime := info.Uptime
		if uptime == "" {
			uptime = "-"
		}
		desc := info.Description
		if desc == "" {
			desc = "-"
		}
		state := string(info.State)

		if l := len(info.Name); l > nameW {
			nameW = l
		}
		if l := len(state); l > statusW {
			statusW = l
		}
		if l := len(pid); l > pidW {
			pidW = l
		}
		if l := len(uptime); l > uptimeW {
			uptimeW = l
		}
		rows = append(rows, [5]string{info.Name, state, pid, uptime, desc})
	}

	const gap = "  "
	header := fmt.Sprintf("%-*s%s%-*s%s%-*s%s%-*s%s%s",
		nameW, hName, gap, statusW, hStatus, gap, pidW, hPID, gap, uptimeW, hUptime, gap, hDescrip)
	fmt.Println(wrap(colour, ansiBold, header))

	for i, info := range infos {
		r := rows[i]
		// Pad the status text *before* wrapping it in colour codes, so the
		// visible width matches the (uncoloured) header. This is the whole
		// reason the manual layout exists.
		statusCell := fmt.Sprintf("%-*s", statusW, r[1])
		statusCell = wrap(colour, stateColour(info.State), statusCell)
		fmt.Printf("%-*s%s%s%s%-*s%s%-*s%s%s\n",
			nameW, r[0], gap,
			statusCell, gap,
			pidW, r[2], gap,
			uptimeW, r[3], gap,
			r[4])
	}
}

// doAction is the boring "send verb, print message, maybe exit non-zero"
// helper used for everything that isn't status.
func doAction(socketPath string, action string, args []string) {
	cmd := types.Command{Action: action, Args: args}
	resp := sendCommand(socketPath, cmd)

	if !resp.Success {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Message)
		os.Exit(1)
	}

	fmt.Println(resp.Message)
}

// sendCommand opens the socket, fires the JSON command, reads the JSON
// response, and gets out. One request per connection — keeping it dumb
// keeps it debuggable.
func sendCommand(socketPath string, cmd types.Command) types.Response {
	conn, err := ipc.Dial(socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot connect to direktor: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is direktord running? Socket: %s\n", socketPath)
		os.Exit(1)
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "Error sending command: %v\n", err)
		os.Exit(1)
	}

	var resp types.Response
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading response: %v\n", err)
		os.Exit(1)
	}

	return resp
}

func versionString() string {
	if version == "" {
		return "v0.1.0"
	}
	return version
}

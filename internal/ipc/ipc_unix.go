//go:build !windows

// Unix flavour of the IPC plumbing — proper local sockets, the way nature
// intended.
package ipc

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// createListener opens a unix socket at path. Removes any stale file first
// because if direktord crashed last time, the socket file is still sitting
// there and net.Listen would refuse to bind. Supervisor would also do this,
// to its credit, but it occasionally got the order wrong and managed to
// delete a *live* socket from another instance — fucking great.
func createListener(path string) (net.Listener, error) {
	os.Remove(path)

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	// Lock the socket down to owner+group. Anyone who can read it can
	// drive the daemon, so 0770 is about as loose as I'm comfortable
	// with. If you want world-accessible, override SocketMode in config
	// and live with the consequences.
	mode, err := strconv.ParseUint("0770", 8, 32)
	if err == nil {
		os.Chmod(path, os.FileMode(mode))
	}

	return listener, nil
}

// cleanupListener removes the socket file on shutdown. Best-effort: if the
// remove fails, there's nothing useful we can do about it.
func cleanupListener(path string) {
	os.Remove(path)
}

// Dial connects to the IPC socket as a client. Used by direktorctl.
// Errors are wrapped with the path so the user sees *which* socket
// it failed to reach — saves an awful lot of "is direktord running" guessing.
func Dial(path string) (net.Conn, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("connecting to direktor at %s: %w", path, err)
	}
	return conn, nil
}

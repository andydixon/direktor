//go:build windows

// Windows flavour of the IPC plumbing.
//
// Honest disclosure: this is a placeholder. A proper implementation would
// use Windows named pipes (\\.\pipe\direktor) which is the right answer for
// security and ergonomics. But Go's stdlib doesn't expose them directly and
// the third-party packages that do (microsoft/go-winio etc.) drag in a
// bunch of dependency baggage.
//
// So for now we use loopback TCP on a fixed port. That's a slightly shit
// trade-off — anything on the box can connect, including any logged-in
// user's processes — but it gets Windows users running while we sort out
// the named-pipe story properly. If you're deploying direktor on Windows
// in a multi-user environment, please don't, or at least firewall this.
package ipc

import (
	"fmt"
	"net"
)

func createListener(path string) (net.Listener, error) {
	// Hard-coded port. Yes, really. See the apologetic doc comment above.
	listener, err := net.Listen("tcp", "127.0.0.1:9877")
	if err != nil {
		return nil, fmt.Errorf("creating named pipe listener: %w", err)
	}
	return listener, nil
}

// cleanupListener — TCP listeners don't need cleanup, but keep the symbol
// so the cross-platform server code doesn't grow build-tag noise.
func cleanupListener(path string) {
}

// Dial connects to the IPC server on Windows. Same fixed port; same caveats.
func Dial(path string) (net.Conn, error) {
	conn, err := net.Dial("tcp", "127.0.0.1:9877")
	if err != nil {
		return nil, fmt.Errorf("connecting to direktor: %w", err)
	}
	return conn, nil
}

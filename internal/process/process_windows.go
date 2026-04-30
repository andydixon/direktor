//go:build windows

// Windows-specific process bits. Windows doesn't have unix signals, so the
// signalling story is fundamentally different — see sendSignal below for the
// best we can do. We put the child in a new console process group so that
// CTRL_BREAK_EVENT can target it specifically rather than blasting our whole
// console.
package process

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/andydixon/direktor/pkg/types"
)

// setPlatformAttributes — Windows flavour. CREATE_NEW_PROCESS_GROUP is the
// rough analogue of Setpgid on Unix.
func setPlatformAttributes(cmd *exec.Cmd, cfg types.ProcessConfig) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// sendSignal — Windows edition. Almost every "signal" name from the config
// is a Unix concept that simply doesn't exist here. We honour INT by sending
// CTRL_BREAK_EVENT (the closest equivalent), and for everything else we
// give up gracefully and just kill the process. This isn't great, but
// Windows has no graceful-stop story for arbitrary processes; if your
// service needs one, it'll have to listen on a named pipe / socket and
// shut itself down on a message.
func sendSignal(proc *os.Process, signal string) error {
	switch signal {
	case "INT":
		// Send CTRL_BREAK_EVENT to the process group via the kernel32 API.
		// Yes, we have to load it dynamically — Go's syscall package doesn't
		// expose this directly. Such is life on Windows.
		dll := syscall.NewLazyDLL("kernel32.dll")
		generateConsoleCtrlEvent := dll.NewProc("GenerateConsoleCtrlEvent")
		r, _, err := generateConsoleCtrlEvent.Call(
			syscall.CTRL_BREAK_EVENT,
			uintptr(proc.Pid),
		)
		if r == 0 {
			return err
		}
		return nil
	default:
		// No graceful equivalent — straight to Kill. Sorry.
		return proc.Kill()
	}
}

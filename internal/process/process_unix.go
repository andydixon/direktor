//go:build !windows

// Unix-specific process bits. The interesting one is Setpgid: by putting each
// child in its own process group we can blanket-signal the whole tree if it
// spawns its own children. Supervisor didn't do this consistently and as a
// result you'd get ghost grandchildren that survived a "stop" — fucking
// nightmare to debug. So we do.
package process

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/andydixon/direktor/pkg/types"
)

// setPlatformAttributes — Unix flavour. The big thing is the new process
// group; everything else (uid/gid, etc.) we leave alone for now.
func setPlatformAttributes(cmd *exec.Cmd, cfg types.ProcessConfig) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // own process group, so we can kill the whole tree
	}
}

// sendSignal translates the friendly signal name from the config into the
// actual syscall.Signal and fires it. Unknown signals fall back to SIGTERM
// because that's what 99% of programs handle properly.
func sendSignal(proc *os.Process, signal string) error {
	var sig syscall.Signal
	switch signal {
	case "HUP":
		sig = syscall.SIGHUP
	case "INT":
		sig = syscall.SIGINT
	case "QUIT":
		sig = syscall.SIGQUIT
	case "KILL":
		sig = syscall.SIGKILL // nuclear option, no cleanup, you sure?
	case "USR1":
		sig = syscall.SIGUSR1
	case "USR2":
		sig = syscall.SIGUSR2
	default:
		sig = syscall.SIGTERM
	}
	return proc.Signal(sig)
}

// direktord — the daemon. This is the bit that systemd (or whatever) actually
// keeps alive. It loads the config, spins up the supervisor core, opens the
// IPC socket so direktorctl has someone to talk to, opens the HTTP server for
// the web UI, and then sits there waiting for SIGTERM.
//
// The flag set is intentionally tiny. Supervisor went mad with command-line
// options, half of which duplicated config keys; we'd rather you put it in
// the conf file and be done with it.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/andydixon/direktor/internal/ipc"
	"github.com/andydixon/direktor/internal/supervisor"
	"github.com/andydixon/direktor/internal/web"
)

// version gets stamped in at build time via -ldflags "-X main.version=...".
// If you're running an unstamped binary, you'll get this fallback. Not ideal
// but at least it's something.
var version = "v0.1.0"

func main() {
	// Tiny flag set on purpose. Everything else lives in the config file —
	// that's the whole point of having one.
	configPath := flag.String("c", "/etc/direktor/direktor.conf", "Path to configuration file")
	nodaemon := flag.Bool("n", false, "Run in foreground (no daemon)")
	showVersion := flag.Bool("v", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("direktord", versionString())
		os.Exit(0)
	}

	// Load config + build the supervisor core. If the config is broken we
	// die here, loudly, before opening any sockets — much easier to debug
	// than a daemon that limps along with half a config.
	sup, err := supervisor.New(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	cfg := sup.Config()

	// Command-line -n wins over whatever the config says. Mostly because
	// systemd unit files invoke us with -n and we'd like that to Just Work
	// even if someone left nodaemon=false in the file.
	if *nodaemon {
		cfg.Supervisor.Nodaemon = true
	}

	// IPC server — this is what direktorctl talks to over a unix socket.
	// If this fails to start, there's no point continuing because nobody
	// can drive the daemon.
	ipcServer := ipc.NewServer(sup, cfg.Supervisor.SocketPath, sup.Logger())
	if err := ipcServer.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting IPC server: %v\n", err)
		os.Exit(1)
	}
	defer ipcServer.Stop()

	// HTTP server — the pretty web UI, plus the JSON API. Non-fatal: if
	// the port's already taken or we can't bind for whatever reason, log
	// and crack on. Supervisor's XML-RPC thing was so often broken that
	// people stopped relying on it; we'd rather degrade than refuse.
	httpServer := web.NewServer(sup, sup.Logger())
	if err := httpServer.Start(); err != nil {
		sup.Logger().Warn("HTTP server failed to start", "error", err)
	} else {
		defer httpServer.Stop()
	}

	// Right, off we go. This kicks off all the autostart processes.
	if err := sup.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting supervisor: %v\n", err)
		os.Exit(1)
	}

	sup.Logger().Info("direktor is running",
		"config", *configPath,
		"http", fmt.Sprintf("%s:%d", cfg.Supervisor.HTTPHost, cfg.Supervisor.HTTPPort),
		"socket", cfg.Supervisor.SocketPath,
	)

	// Park here until something (signal handler, IPC shutdown command,
	// asteroid impact) tells us we're done.
	sup.Wait()
}

// versionString returns the version string with a fallback for unstamped builds.
// Belt-and-braces — `version` is already initialised above, but if someone
// ever sets it to "" via -ldflags this stops us printing nothing.
func versionString() string {
	if version == "" {
		return "v0.1.0"
	}
	return version
}

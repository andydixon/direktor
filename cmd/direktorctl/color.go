// ANSI colour helpers for direktorctl. We honour NO_COLOR (the de facto
// standard, see no-color.org), DIREKTOR_COLOR (the explicit override), and
// otherwise fall back to "is stdout a terminal?".
//
// The point: piping `direktorctl status | grep RUNNING` shouldn't smear escape
// codes through your grep results. Supervisor never bothered with this — its
// CLI just splatted colour at you regardless — so half the world has shell
// scripts full of `sed 's/\x1b\[[0-9;]*m//g'` to clean up after it. Sigh.
package main

import (
	"os"
	"strings"

	"github.com/andydixon/direktor/pkg/types"
)

// The bare minimum ANSI palette. We don't try to be clever with 256-colour
// or truecolour; basic ANSI works on absolutely everything.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m" // we call it "amber" in the docs because it reads better
	ansiGrey   = "\x1b[90m"
)

// useColour decides whether we should be emitting escape codes at all.
//
// Order of resolution (first match wins):
//  1. DIREKTOR_COLOR=never|always — explicit override, end of story.
//  2. NO_COLOR is set to anything — disable.
//  3. stdout is a TTY — enable.
//  4. anything else (file, pipe, dodgy stat) — disable.
//
// Yes, an `os.Stat` error means "no colour" — better silent than confusing.
func useColour() bool {
	switch strings.ToLower(os.Getenv("DIREKTOR_COLOR")) {
	case "never", "off", "no", "0", "false":
		return false
	case "always", "on", "yes", "1", "true", "force":
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// stateColour maps a process state to an ANSI colour. Roughly:
//
//	RUNNING            → green   (good, leave it alone)
//	STARTING/BACKOFF/STOPPING → amber (in flux, watch it)
//	EXITED/FATAL       → red     (bad, look at this)
//	STOPPED/UNKNOWN    → grey    (deliberately quiet, not interesting)
//
// The default branch returns grey for the same reason — anything we don't
// recognise probably isn't actionable, so don't shout about it.
func stateColour(state types.ProcessState) string {
	switch state {
	case types.StateRunning:
		return ansiGreen
	case types.StateStarting, types.StateBackoff, types.StateStopping:
		return ansiYellow
	case types.StateExited, types.StateFatal:
		return ansiRed
	case types.StateStopped, types.StateUnknown:
		return ansiGrey
	default:
		return ansiGrey
	}
}

// wrap returns s wrapped in the given ANSI codes — but only if colour is on
// and the code is non-empty. Saves callers from `if useColour() { ... }`
// scaffolding everywhere.
func wrap(enabled bool, code, s string) string {
	if !enabled || code == "" {
		return s
	}
	return code + s + ansiReset
}

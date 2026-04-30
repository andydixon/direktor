// scripts/build is the cross-compile helper. Spits out per-OS/per-arch
// directories under dist/ containing direktord and direktorctl binaries.
//
// Why a Go program rather than a Makefile or shell script? Because then it
// works on Windows, macOS, and Linux without anyone having to install GNU
// make / coreutils. `go run ./scripts/build` is the lowest-friction entry
// point we can offer.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// target is one OS/arch tuple. Plain struct, not stringly-typed, because
// we pass these around enough that catching typos at compile time is nice.
type target struct {
	goos   string
	goarch string
}

// binary is one cmd we know how to build. Name = output filename
// (.exe gets bolted on for Windows in buildBinary), pkg = the import path.
type binary struct {
	name string
	pkg  string
}

var (
	// The "build everything we ship for" set, used when --targets is empty.
	// If you add an architecture here, also remember the install scripts in
	// deploy/ — they assume these names.
	defaultTargets = []target{
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
		{goos: "windows", goarch: "amd64"},
		{goos: "windows", goarch: "arm64"},
	}
	binaries = []binary{
		{name: "direktord", pkg: "./cmd/direktord"},
		{name: "direktorctl", pkg: "./cmd/direktorctl"},
	}
)

func main() {
	var (
		outputDir    string
		targetsValue string
		version      string
		release      bool
	)

	flag.StringVar(&outputDir, "output", "dist", "Output directory")
	flag.StringVar(&targetsValue, "targets", "", "Comma-separated build targets in GOOS/GOARCH form")
	flag.StringVar(&version, "version", "", "Optional version string embedded via -ldflags")
	flag.BoolVar(&release, "release", true, "Strip debug symbols for release builds")
	flag.Parse()

	targets, err := parseTargets(targetsValue)
	if err != nil {
		fail(err)
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		fail(err)
	}

	// Normalise the output dir to absolute under repoRoot. This means
	// `go run ./scripts/build` from anywhere in the repo Just Works,
	// rather than scattering dist/ folders all over the place.
	outputDir = filepath.Clean(outputDir)
	if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join(repoRoot, outputDir)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		fail(fmt.Errorf("create output directory: %w", err))
	}

	if len(targets) == 0 {
		targets = defaultTargets
	}

	ldflags := buildLDFlags(version, release)
	for _, t := range targets {
		for _, bin := range binaries {
			if err := buildBinary(repoRoot, outputDir, bin, t, ldflags); err != nil {
				fail(err)
			}
		}
	}

	fmt.Printf("Built %d targets into %s\n", len(targets), outputDir)
}

// parseTargets parses the --targets flag (e.g. "linux/amd64,darwin/arm64").
// Empty / whitespace returns nil so the caller can fall back to defaults.
func parseTargets(raw string) ([]target, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	targets := make([]target, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		segments := strings.Split(part, "/")
		if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
			return nil, fmt.Errorf("invalid target %q, expected GOOS/GOARCH", part)
		}
		targets = append(targets, target{goos: segments[0], goarch: segments[1]})
	}

	return targets, nil
}

// findRepoRoot walks up from cwd looking for go.mod. If we can't find one
// we're not in a Go project and there's nothing useful to do.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Hit the filesystem root without finding go.mod. Give up.
			return "", fmt.Errorf("could not find repository root from %s", dir)
		}
		dir = parent
	}
}

// buildLDFlags assembles the -ldflags string. -s -w strips symbols for
// release builds (smaller binary, less reverse-engineering surface);
// -X main.version=... stamps the version constant in main.
func buildLDFlags(version string, release bool) string {
	var flags []string
	if release {
		flags = append(flags, "-s", "-w")
	}
	if version != "" {
		flags = append(flags, fmt.Sprintf("-X main.version=%s", version))
	}
	return strings.Join(flags, " ")
}

// buildBinary shells out to `go build` for one (target, binary) pair.
// CGO is disabled — keeps things statically linked and dodges glibc-version
// portability headaches. If you need CGO for something specific, sigh, and
// run `go build` by hand.
func buildBinary(repoRoot string, outputDir string, bin binary, t target, ldflags string) error {
	targetDir := filepath.Join(outputDir, fmt.Sprintf("%s-%s", t.goos, t.goarch))
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create target directory %s: %w", targetDir, err)
	}

	name := bin.name
	if t.goos == "windows" {
		name += ".exe"
	}
	outPath := filepath.Join(targetDir, name)

	args := []string{"build"}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "-o", outPath, bin.pkg)

	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+t.goos,
		"GOARCH="+t.goarch,
	)

	fmt.Printf("[%s/%s] building %s\n", t.goos, t.goarch, bin.name)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s for %s/%s: %w", bin.name, t.goos, t.goarch, err)
	}

	return nil
}

// fail prints to stderr and exits non-zero. One-stop error termination.
func fail(err error) {
	fmt.Fprintln(os.Stderr, "build error:", err)
	os.Exit(1)
}

// init customises the usage message on Windows so the example uses
// backslashes (`go run .\scripts\build`) — looks more native to a
// PowerShell user staring at it.
func init() {
	if runtime.GOOS == "windows" {
		flag.Usage = func() {
			fmt.Fprintf(flag.CommandLine.Output(), "Usage: go run .\\scripts\\build [flags]\n")
			flag.PrintDefaults()
		}
	}
}

// Package logging gives us two things: a thin wrapper around log/slog (so
// the rest of the codebase has a single Logger type to import), and a
// rotating file writer for managed-process stdout/stderr.
//
// The slog wrapper is mostly cosmetic — slog is already perfectly good —
// but it keeps the import surface neat. The rotating writer is the more
// interesting half: supervisor outsourced log rotation to logrotate(8) and
// got into all sorts of bother with file descriptors when logrotate would
// truncate a file out from under a long-running child. Doing it ourselves
// means we control the swap and the child never has its fd yanked.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger wraps *slog.Logger. Right now the wrapper does nothing; it exists
// so we can add cross-cutting behaviour later (request IDs, sampled debug
// logging, etc.) without changing every caller.
type Logger struct {
	*slog.Logger
}

// New constructs a Logger that emits JSON to the given writer. Level is
// the string from config — "debug", "info", "warn"/"warning", "error".
// Anything we don't recognise falls back to info, on the theory that
// spamming someone with debug because they typoed "infoo" is unhelpful.
func New(level string, output io.Writer) *Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: lvl,
	})

	return &Logger{Logger: slog.New(handler)}
}

// RotatingWriter is an io.WriteCloser that rotates the underlying file once
// it crosses maxBytes. backups is how many `.1`, `.2`, ... files to keep.
//
// It's deliberately minimal: size-based only, no time-based rotation, no
// compression. If you need either, pipe through logrotate (carefully — see
// the package doc) or a sidecar tool. Ninety percent of users want
// "rotate at 50MB, keep 10 backups" and that's what this does well.
type RotatingWriter struct {
	mu          sync.Mutex
	file        *os.File
	path        string
	maxBytes    int64
	backups     int
	currentSize int64
}

// NewRotatingWriter creates the writer. Creates the directory tree if it
// doesn't exist (0755 — owner write, world read+execute, the usual).
func NewRotatingWriter(path string, maxBytes int64, backups int) (*RotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	w := &RotatingWriter{
		path:     path,
		maxBytes: maxBytes,
		backups:  backups,
	}

	if err := w.openFile(); err != nil {
		return nil, err
	}

	return w, nil
}

// openFile (re)opens the log file in append mode and seeds currentSize from
// the existing length on disk. Used both at construction and after a rotate.
func (w *RotatingWriter) openFile() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("opening log file %s: %w", w.path, err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}

	w.file = f
	w.currentSize = info.Size()
	return nil
}

// Write — io.Writer with rotation. We check size *before* writing, because
// that way we never write across a rotation boundary. The trade-off: a
// single Write that's bigger than maxBytes will still go in one piece (we
// don't try to chop it). That's fine — log lines are usually small, and
// "one rotation lagged by one giant line" is much less surprising than
// "log line cut in half".
func (w *RotatingWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.maxBytes > 0 && w.currentSize+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err = w.file.Write(p)
	w.currentSize += int64(n)
	return n, err
}

// rotate closes the current file, shuffles existing backups down by one
// (.1 → .2, .2 → .3, etc.), moves the live file to .1, and reopens. If
// backups is 0 we just remove the file outright.
//
// Yes, the os.Rename / os.Remove errors are deliberately ignored — there's
// nothing useful we can do if rotation half-fails, and surfacing the error
// would just kill the writer (and therefore the process) for a problem
// that's almost always transient (e.g. the operator's sat in /var/log
// with `ls`). Best-effort is the only sensible policy here.
func (w *RotatingWriter) rotate() error {
	w.file.Close()

	for i := w.backups - 1; i > 0; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		os.Rename(src, dst)
	}

	if w.backups > 0 {
		os.Rename(w.path, fmt.Sprintf("%s.1", w.path))
	} else {
		os.Remove(w.path)
	}

	return w.openFile()
}

// Close — io.Closer. Safe to call once; subsequent calls return nil because
// w.file gets nil-checked.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// ProcessLogWriter wraps another writer and prepends a timestamp + prefix
// to each Write. Used when you want the supervisor's view of the process
// output (with timestamps) rather than just the raw byte stream.
//
// Caveat: it timestamps each Write call, not each *line*. If a child does
// `printf("hello "); printf("world\n")` you'll get one line stamped with
// the time of the first printf, not two stamps. Good enough for most
// real-world logging, but worth knowing.
type ProcessLogWriter struct {
	writer io.Writer
	prefix string
	mu     sync.Mutex
}

// NewProcessLogWriter — small convenience constructor.
func NewProcessLogWriter(w io.Writer, prefix string) *ProcessLogWriter {
	return &ProcessLogWriter{writer: w, prefix: prefix}
}

// Write prepends "YYYY-MM-DD HH:MM:SS.mmm [prefix] " and forwards to the
// underlying writer. Returns len(data) on success rather than the bytes-
// actually-written count, because callers only care about whether their
// data made it through; the stamp bytes are bookkeeping.
func (p *ProcessLogWriter) Write(data []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	line := fmt.Sprintf("%s [%s] %s", timestamp, p.prefix, string(data))
	_, err = p.writer.Write([]byte(line))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

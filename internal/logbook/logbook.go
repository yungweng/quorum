// Package logbook writes quorum's log file.
//
// The file is the record of what the agent did while nobody was watching, so
// it has to survive unattended for months: it rotates itself, and every write
// is opened and closed so two processes appending at once cannot interleave a
// partial line.
package logbook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxBytes is the size at which the log is rotated to <name>.1.
const MaxBytes = 1 << 20

// Logger appends timestamped lines to a file, and to a terminal when one is
// attached.
type Logger struct {
	Path string
	// Echo receives the same message without the timestamp. Nil prints nothing.
	Echo func(string)

	mu sync.Mutex
}

func New(path string) *Logger { return &Logger{Path: path} }

// Printf writes one line. Failures to log are swallowed: quorum has nowhere
// better to report them, and losing a log line must never stop a review.
func (l *Logger) Printf(format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.Echo != nil {
		l.Echo(msg)
	}
	if l.Path == "" {
		return
	}
	l.rotate()
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

// rotate moves the log aside once it grows past MaxBytes, keeping exactly one
// previous file. The caller holds the mutex.
func (l *Logger) rotate() {
	fi, err := os.Stat(l.Path)
	if err != nil || fi.Size() < MaxBytes {
		return
	}
	os.Rename(l.Path, l.Path+".1")
}

// Tail returns the last n lines of the log, oldest first.
func (l *Logger) Tail(n int) []string {
	b, err := os.ReadFile(l.Path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yungweng/quorum/internal/proc"
)

// A marker is the proof that a review is in flight. Reviews run as detached
// processes, so the marker is what stops two polls from starting the same pull
// request and what bounds how many run at once. Creating it is exclusive, which
// is the whole point: whoever manages to create the file owns the review.
type Marker struct {
	Key  string
	Path string
	PID  int
}

// SafeName turns "owner/repo#123" into a single path element.
func SafeName(key string) string {
	return strings.NewReplacer("/", "-", "#", "-").Replace(key)
}

// KeyFromSafeName is not possible in general, so markers carry their key in the
// file. This reads it back.
func readMarker(path string) (Marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Marker{}, false
	}
	lines := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return Marker{}, false
	}
	m := Marker{Path: path, PID: pid}
	if len(lines) > 1 {
		m.Key = strings.TrimSpace(lines[1])
	}
	return m, true
}

// Acquire claims the review of key, or reports false when somebody already
// holds it. A marker whose process is gone is treated as free.
func Acquire(dir, key string) (*Marker, bool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, SafeName(key))
	for attempt := range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), key)
			f.Close()
			return &Marker{Key: key, Path: path, PID: os.Getpid()}, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, err
		}
		existing, ok := readMarker(path)
		if ok && proc.Alive(existing.PID) {
			return nil, false, nil
		}
		// Stale: the holder died. Clear it and try once more.
		if attempt == 0 {
			os.Remove(path)
		}
	}
	return nil, false, nil
}

// Adopt rewrites the marker with the pid that actually runs the review, which
// is the detached child rather than the poll that created the marker.
func (m *Marker) Adopt(pid int) error {
	m.PID = pid
	return os.WriteFile(m.Path, []byte(fmt.Sprintf("%d\n%s\n", pid, m.Key)), 0o644)
}

func (m *Marker) Release() {
	if m != nil && m.Path != "" {
		os.Remove(m.Path)
	}
}

// Live lists the reviews actually running, deleting markers left behind by
// processes that are gone.
func Live(dir string) []Marker {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Marker
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		m, ok := readMarker(path)
		if !ok || !proc.Alive(m.PID) {
			os.Remove(path)
			continue
		}
		out = append(out, m)
	}
	return out
}

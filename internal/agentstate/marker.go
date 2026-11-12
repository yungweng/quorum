// Package agentstate holds the small files the agent keeps beside its state:
// the markers of reviews in flight, the usage-limit pause and the heartbeat
// of the last finished poll. It also reads the load average the poll throttles
// on.
package agentstate

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

// draftPrefix names a marker that is still being written. Live skips those; a
// repository owner cannot start with a dot, so no real marker does.
const draftPrefix = ".draft-"

// safeName turns "owner/repo#123" into a single path element.
func safeName(key string) string {
	return strings.NewReplacer("/", "-", "#", "-").Replace(key)
}

// safeName cannot be reversed in general, so markers carry their key in the
// file.
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
	path := filepath.Join(dir, safeName(key))
	// The marker has to appear with its content already in it. Created empty
	// and written afterwards, it could be read in between by Live, which
	// deletes what it cannot parse, and the next poll then started the same
	// review a second time. A hard link is as exclusive as O_EXCL and only
	// ever shows the finished file.
	draft, err := os.CreateTemp(dir, draftPrefix)
	if err != nil {
		return nil, false, err
	}
	defer os.Remove(draft.Name())
	_, err = fmt.Fprintf(draft, "%d\n%s\n", os.Getpid(), key)
	if closeErr := draft.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, false, err
	}
	for attempt := range 2 {
		err := os.Link(draft.Name(), path)
		if err == nil {
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
	// Replaced in one rename, for the reason Acquire links: a marker caught
	// truncated is a marker deleted. Only the holder adopts, so one draft name
	// per marker is enough.
	draft := filepath.Join(filepath.Dir(m.Path), draftPrefix+filepath.Base(m.Path))
	if err := os.WriteFile(draft, []byte(fmt.Sprintf("%d\n%s\n", pid, m.Key)), 0o644); err != nil {
		os.Remove(draft)
		return err
	}
	if err := os.Rename(draft, m.Path); err != nil {
		os.Remove(draft)
		return err
	}
	return nil
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
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
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

// LiveKeys is the set of keys Live reports.
func LiveKeys(dir string) map[string]bool {
	keys := map[string]bool{}
	for _, m := range Live(dir) {
		keys[m.Key] = true
	}
	return keys
}

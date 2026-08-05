package runner

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PauseFile is the state-dir marker that records an engine usage-limit pause.
// While its timestamp lies in the future, the poll starts no new reviews.
const PauseFile = "codex-paused-until"

// defaultUsageLimitPause covers a refusal whose reset time could not be
// parsed: long enough to stop the hammering, short enough to notice when the
// account comes back early.
const defaultUsageLimitPause = time.Hour

// WritePause records that the engine should not be asked to start new work
// before until. wrote reports whether this call moved the pause forward:
// several reviews can hit the limit within moments of each other, and only
// the one that extends the deadline is worth a notification.
func WritePause(stateDir string, until time.Time) (bool, error) {
	unlock, err := lockPath(filepath.Join(stateDir, PauseFile+".lock"))
	if err != nil {
		return false, err
	}
	defer unlock()

	if current, ok := readPauseFile(stateDir); ok && !until.After(current) {
		return false, nil
	}
	path := filepath.Join(stateDir, PauseFile)
	if err := os.WriteFile(path, []byte(until.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// ReadPause reports the active pause, if any. A missing file, an unparsable
// one, or one whose time has passed all count as "not paused".
func ReadPause(stateDir string) (time.Time, bool) {
	until, ok := readPauseFile(stateDir)
	if !ok || !until.After(time.Now()) {
		return time.Time{}, false
	}
	return until, true
}

func readPauseFile(stateDir string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(stateDir, PauseFile))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

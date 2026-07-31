package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/yungweng/quorum/internal/proc"
)

// ProgressFile is what a run publishes its position in, inside its run
// directory.
const ProgressFile = "progress.json"

// Phases a run reports. A phase is what the pipeline is blocked on, which is
// not always the only thing happening: the first review runs in the background
// while the pipeline waits out the initial CI.
const (
	PhaseStarting = "starting"
	PhaseCI       = "ci"
	PhaseReview   = "review"
	PhaseFix      = "fix"
	PhaseCIFix    = "ci-fix"
)

// Check states the progress file records. Empty means nothing is known yet.
const (
	CIWaiting = "waiting"
	CIGreen   = "green"
	CIRed     = "red"
	CINone    = "none"
)

// Progress is what a run publishes about itself while it runs.
//
// It exists for the same reason internal/review writes an events log: the
// dashboard has to follow a run it did not start and cannot call back into. A
// run started from a terminal never touches the state file, so for those this
// is the only trace there is.
//
// It is deliberately small and rewritten whole on every change. The Codex logs
// beside it reach megabytes within minutes, and a dashboard that redraws every
// three seconds cannot parse those.
type Progress struct {
	// PID is the process the pipeline runs in. For a run the agent started
	// that is the same process its marker names, which is what lets the
	// dashboard tell its own record apart from an unrelated review of the same
	// pull request.
	PID int `json:"pid"`

	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	Branch string `json:"branch,omitempty"`

	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Round      int `json:"round,omitempty"`
	MaxIter    int `json:"max_iter,omitempty"`
	MaxCIFixes int `json:"max_ci_fixes,omitempty"`

	// Phase is what the run is doing now, Since when it started doing it.
	Phase string    `json:"phase,omitempty"`
	Since time.Time `json:"since"`

	// CI is the last check state seen, and CIFix which repair attempt is under
	// way after a red one.
	CI    string `json:"ci,omitempty"`
	CIFix int    `json:"ci_fix,omitempty"`

	// Reviewed goes true with the first round result, which is what tells "no
	// findings yet" apart from "no findings".
	Reviewed bool `json:"reviewed,omitempty"`
	Blockers int  `json:"blockers,omitempty"`
	Critical int  `json:"critical,omitempty"`

	Commits int `json:"commits,omitempty"`

	// RunDir is filled in by the reader from where the file was found, so a
	// moved or copied run cache cannot report a path that is not there.
	RunDir string `json:"-"`
}

// Key identifies either a pull request or a branch-only run.
func (p Progress) Key() string {
	if p.Number > 0 {
		return fmt.Sprintf("%s#%d", p.Repo, p.Number)
	}
	return p.Repo + "@" + p.Branch
}

// LogDir is where the run's Codex logs are.
func (p Progress) LogDir() string { return filepath.Join(p.RunDir, "logs") }

// publish rewrites the run's progress file.
//
// A reader may be reading it at any moment, so it is replaced in one rename
// rather than truncated and written in place: a dashboard that caught it
// half-written would show a run that does not exist. Failures are silent on
// purpose. Progress is decoration, and a run must not die because a cache
// directory went read-only.
func (r *run) publish() {
	if r.root == "" {
		return
	}
	r.prog.UpdatedAt = time.Now()
	b, err := json.Marshal(r.prog)
	if err != nil {
		return
	}
	tmp := filepath.Join(r.root, fmt.Sprintf(".%s.%d", ProgressFile, os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(r.root, ProgressFile)); err != nil {
		os.Remove(tmp)
	}
}

// enter records a change of phase and stamps when it began, which is what lets
// a watcher show how long a run has been in one place.
func (r *run) enter(phase string) {
	r.prog.Phase = phase
	r.prog.Since = time.Now()
	r.publish()
}

// ReadProgress reads one run's progress file. A run that has not published yet
// simply reports nothing.
func ReadProgress(runDir string) (Progress, bool) {
	b, err := os.ReadFile(filepath.Join(runDir, ProgressFile))
	if err != nil {
		return Progress{}, false
	}
	var p Progress
	if err := json.Unmarshal(b, &p); err != nil {
		return Progress{}, false
	}
	p.RunDir = runDir
	return p, true
}

// LiveRuns lists the runs under dir that are still going, oldest first.
//
// Liveness is the run directory's claim, which is the same test the run cache's
// own collection uses. A run reported here therefore can never be one that
// collection is about to remove, and a run whose process was killed drops off
// by itself when the kernel releases its lock.
func LiveRuns(dir string) []Progress {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Progress
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runDir := filepath.Join(dir, e.Name())
		if !proc.Claimed(runDir) {
			continue
		}
		// A run claims its directory before it publishes, so for a moment a
		// live run has no progress file. Skipping it costs one redraw.
		if p, ok := ReadProgress(runDir); ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].RunDir < out[j].RunDir
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

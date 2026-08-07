package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/proc"
)

// LiveRun describes a review that is still running outside the agent.
type LiveRun struct {
	PID       int       `json:"pid"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Branch    string    `json:"branch,omitempty"`
	Title     string    `json:"title,omitempty"`
	Author    string    `json:"author,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Reviewers int       `json:"reviewers"`

	// Model is what every pass of this run works on, resolved. A watcher would
	// otherwise have to read the current configuration, which can have changed
	// since this run started.
	Model engine.Model `json:"model,omitzero"`

	RunDir string `json:"run_dir"`
}

// Key identifies either a pull request or a branch-only run.
func (r LiveRun) Key() string {
	if r.Number > 0 {
		return fmt.Sprintf("%s#%d", r.Repo, r.Number)
	}
	return r.Repo + "@" + r.Branch
}

// LiveTracker publishes one terminal review until Close removes its record.
type LiveTracker struct {
	Reporter
	started time.Time
	dir     string
	path    string
}

// TrackLive wraps the reporter used by a terminal review. It publishes enough
// metadata for quorum watch to show that run without writing scheduler state.
func TrackLive(rep Reporter, dir string) *LiveTracker {
	return &LiveTracker{Reporter: rep, started: time.Now(), dir: dir}
}

func (r *LiveTracker) Header(h RunHeader) {
	r.path = filepath.Join(r.dir, fmt.Sprintf("%d.json", os.Getpid()))
	err := publishLive(r.dir, r.path, LiveRun{
		PID:       os.Getpid(),
		Repo:      h.Repo,
		Number:    h.Number,
		Branch:    h.Branch,
		Title:     h.Title,
		Author:    h.Author,
		StartedAt: r.started,
		Reviewers: h.Runs,
		Model:     engine.Model{Engine: h.Engine, Name: h.Model, Effort: h.Effort},
		RunDir:    h.RunDir,
	})
	r.Reporter.Header(h)
	if err != nil {
		r.Warn(fmt.Sprintf("dashboard tracking unavailable: %v", err))
	}
}

// Close removes the live record on every normal exit, including failed runs.
// A crashed process leaves one stale file, which LiveRuns removes after its
// process or run claim disappears.
func (r *LiveTracker) Close() {
	if r.path != "" {
		_ = os.Remove(r.path)
	}
}

// publishLive replaces the status file atomically. A dashboard may read it
// while Header writes it, and a partial JSON document must look absent rather
// than corrupt.
func publishLive(dir, path string, run LiveRun) error {
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%d.json", os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readLive(path string) (LiveRun, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return LiveRun{}, false
	}
	var run LiveRun
	if err := json.Unmarshal(b, &run); err != nil {
		return LiveRun{}, false
	}
	if run.PID < 1 || run.Repo == "" || (run.Number < 1 && run.Branch == "") || run.Reviewers < 1 ||
		run.StartedAt.IsZero() || run.RunDir == "" {
		return LiveRun{}, false
	}
	return run, true
}

// LiveRuns lists terminal review records whose process and run claim still
// exist, oldest first. The directory contains only these tiny records, not the
// run cache.
func LiveRuns(dir string) []LiveRun {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []LiveRun
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		run, ok := readLive(path)
		if !ok {
			continue
		}
		if !proc.Alive(run.PID) || !proc.Claimed(run.RunDir) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].RunDir < out[j].RunDir
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

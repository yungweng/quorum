// Package history is the log of what quorum has done, one entry per run.
//
// It exists because neither of the other two stores can answer that question.
// The state file holds one record per pull request, so five review runs on the
// same pull request overwrite each other and only the last survives; and it is
// written by the agent alone, so a `quorum review` started in a terminal never
// appears in it at all. The run cache does keep every run, but a run directory
// is named after the repository with its slash flattened to a hyphen, which
// cannot be parsed back into an owner and a repository, and it is collected
// once the cache budget is reached.
//
// So runs report themselves here as they finish. Entries are small and the log
// is capped, which keeps it cheap enough for a dashboard that repaints every
// second to read the whole thing every time.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kind is what was run.
const (
	KindReview  = "review"
	KindBabysit = "babysit"
)

// Source is who started it.
const (
	SourceAgent  = "agent"
	SourceManual = "manual"
)

// Outcome values. They are deliberately the same words the state file uses,
// so a reader does not have to translate between the two.
const (
	OK        = "ok"
	Failed    = "failed"
	Skipped   = "skipped"
	GaveUp    = "gave-up"
	Converged = "converged"
)

// Run is one finished run.
type Run struct {
	Key       string    `json:"key"` // "owner/repo#42"
	Title     string    `json:"title,omitempty"`
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at"`

	Blockers    int `json:"blockers,omitempty"`
	Critical    int `json:"critical,omitempty"`
	Suggestions int `json:"suggestions,omitempty"`
	Questions   int `json:"questions,omitempty"`

	// Reviewed distinguishes a run that produced counts of zero from one that
	// never got as far as a review, since both leave the counts at zero.
	Reviewed bool `json:"reviewed,omitempty"`

	Rounds     int    `json:"rounds,omitempty"`
	CommentURL string `json:"comment_url,omitempty"`
	RunDir     string `json:"run_dir,omitempty"`
}

// Name is the "owner/repo" part of the key, and Number the pull request.
func (r Run) Name() string {
	repo, _, _ := strings.Cut(r.Key, "#")
	if _, name, ok := strings.Cut(repo, "/"); ok {
		return name
	}
	return repo
}

func (r Run) Number() int {
	_, number, ok := strings.Cut(r.Key, "#")
	if !ok {
		return 0
	}
	n := 0
	if _, err := fmt.Sscanf(number, "%d", &n); err != nil {
		return 0
	}
	return n
}

// URL is the pull request this run was about.
func (r Run) URL() string {
	repo, number, ok := strings.Cut(r.Key, "#")
	if !ok {
		return ""
	}
	return "https://github.com/" + repo + "/pull/" + number
}

// maxEntries is how many entries the log keeps.
//
// The dashboard shows at most a few dozen, but a longer tail costs almost
// nothing and means raising the configured count does not start from an empty
// screen. At roughly two hundred bytes an entry this is well under a hundred
// kilobytes, which is what keeps reading the whole file every repaint cheap.
const maxEntries = 500

// Append adds one run to the log, trimming it back to cap.
//
// A failure here is not worth failing a run over: the run itself already
// happened and its output is on disk. Callers log and carry on.
func Append(path string, run Run) error {
	if path == "" {
		return errors.New("no history log configured")
	}
	if run.EndedAt.IsZero() {
		run.EndedAt = time.Now()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	unlock, err := lock(path)
	if err != nil {
		return err
	}
	defer unlock()

	runs := readFile(path)
	runs = append(runs, run)
	if len(runs) > maxEntries {
		runs = runs[len(runs)-maxEntries:]
	}
	return writeFile(path, runs)
}

// Read returns the newest limit runs, newest first. A limit of zero or less
// returns everything.
func Read(path string, limit int) []Run {
	runs := readFile(path)
	// Stored oldest first, because appending is what happens most often.
	out := make([]Run, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		out = append(out, runs[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// readFile returns the log oldest first. A line that will not parse is
// skipped rather than failing the read: one truncated entry must not hide
// every run before it.
func readFile(path string) []Run {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var runs []Run
	s := bufio.NewScanner(f)
	// Entries are one line each and small, but a pull request title is
	// arbitrary text, so give the scanner room rather than stopping at a long
	// line and losing the rest of the file.
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var run Run
		if json.Unmarshal(line, &run) != nil || run.Key == "" {
			continue
		}
		runs = append(runs, run)
	}
	return runs
}

// writeFile replaces the log atomically, so a dashboard reading it while it is
// rewritten sees either the old file or the new one.
func writeFile(path string, runs []Run) error {
	var b strings.Builder
	for _, run := range runs {
		line, err := json.Marshal(run)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

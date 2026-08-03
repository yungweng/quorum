package review

import (
	"encoding/json"
	"os"
	"time"
)

// RunHeader is what a run announces about itself before it starts.
type RunHeader struct {
	Repo        string
	Number      int
	Title       string
	Author      string
	Branch      string
	BranchOnly  bool
	BaseRef     string
	BaseSHA     string
	HeadSHA     string
	Draft       bool
	Runs        int
	Concurrency int
	Timeout     time.Duration
	Model       string
	Effort      string
	RunDir      string
}

// Reporter receives everything a run wants to say. It exists so the same core
// can render as a full terminal report under `quorum review`, as a single
// status line inside a fix round, and as nothing at all under the daemon,
// without any of that leaking into the review logic.
type Reporter interface {
	Header(RunHeader)
	Info(string)
	Warn(string)
	// Progress is called about once a second while reviewers run.
	Progress(done, failed, running int, elapsed time.Duration)
	// ReviewerStarted is called when a reviewer pass actually begins, which
	// with a concurrency below the number of passes is not when the run does.
	// Without it a live display cannot tell a reviewer that is working from
	// one that is still queued behind the semaphore.
	ReviewerStarted(idx int)
	ReviewerDone(idx int, elapsed time.Duration, rank, total int)
	ReviewerFailed(idx, exitCode int, elapsed time.Duration, logPath string)
	Aggregating(reviewers, attempt int)
	Verifying(attempt int)
	// Comment is called with the path to the finished comment, before posting.
	Comment(path string)
	Done(Result)
}

// NopReporter discards everything. It is what the daemon uses: its progress
// comes from the state file, which the dashboard reads.
type NopReporter struct{}

func (NopReporter) Header(RunHeader)                                   {}
func (NopReporter) Info(string)                                        {}
func (NopReporter) Warn(string)                                        {}
func (NopReporter) Progress(_, _, _ int, _ time.Duration)              {}
func (NopReporter) ReviewerStarted(int)                                {}
func (NopReporter) ReviewerDone(_ int, _ time.Duration, _, _ int)      {}
func (NopReporter) ReviewerFailed(_, _ int, _ time.Duration, _ string) {}
func (NopReporter) Aggregating(_, _ int)                               {}
func (NopReporter) Verifying(int)                                      {}
func (NopReporter) Comment(string)                                     {}
func (NopReporter) Done(Result)                                        {}

// writeFindings persists the machine-readable summary next to the comment.
// Scripts and the fix pipeline branch on this instead of parsing Markdown.
func writeFindings(path string, f Findings) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

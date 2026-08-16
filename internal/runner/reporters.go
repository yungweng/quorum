package runner

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/review"
)

// logReporter renders a review run into the run's log file. An automated review
// has no terminal, so there is no spinner and no redrawing: every line is
// permanent and timestamped by the log itself.
type logReporter struct {
	mu sync.Mutex
	w  io.Writer
	// onRunDir fires once, as soon as the run directory is known, so the
	// dashboard can follow the reviewers while the review is still going.
	onRunDir func(string)
}

func (l *logReporter) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, format+"\n", args...)
}

func (l *logReporter) Header(h review.RunHeader) {
	l.printf("reviewing %s#%d %q by @%s", h.Repo, h.Number, h.Title, h.Author)
	l.printf("base %s (%s), head %s", h.BaseRef, short(h.BaseSHA), short(h.HeadSHA))
	l.printf("%d reviewers, model %s, effort %s", h.Runs, h.Model, h.Effort)
	l.printf("run dir %s", h.RunDir)
	if l.onRunDir != nil {
		l.onRunDir(h.RunDir)
	}
}

func (l *logReporter) Info(s string) { l.printf("%s", s) }
func (l *logReporter) Warn(s string) { l.printf("warning: %s", s) }

// Progress is dropped: the log is not a terminal, and the dashboard reads the
// events log instead.
func (l *logReporter) Progress(_, _, _ int, _ time.Duration) {}

func (l *logReporter) ReviewerStarted(idx int) { l.printf("reviewer-%d started", idx) }

func (l *logReporter) ReviewerDone(idx int, elapsed time.Duration, rank, total int) {
	l.printf("reviewer-%d done in %s [%d/%d]", idx, elapsed.Round(time.Second), rank, total)
}

func (l *logReporter) ReviewerFailed(idx, code int, elapsed time.Duration, logPath string) {
	l.printf("reviewer-%d failed with exit %d after %s -> %s",
		idx, code, elapsed.Round(time.Second), logPath)
}

func (l *logReporter) Aggregating(reviewers, attempt int) {
	if attempt > 1 {
		l.printf("aggregating %d reviewer output(s), attempt %d", reviewers, attempt)
		return
	}
	l.printf("aggregating %d reviewer output(s)", reviewers)
}

func (l *logReporter) Verifying(attempt int) {
	if attempt > 1 {
		l.printf("verifying findings, attempt %d", attempt)
		return
	}
	l.printf("verifying findings")
}

func (l *logReporter) Comment(path string) { l.printf("comment written to %s", path) }

func (l *logReporter) Done(res review.Result) {
	l.printf("done in %s: %s", res.Duration.Round(time.Second), res.Findings.Summary())
	l.printf("verification changes written to %s", res.VerificationChangesFile)
	if res.Posted {
		l.printf("posted -> %s", res.CommentURL)
	}
}

// loopLogReporter renders a babysit run into the same kind of log.
type loopLogReporter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *loopLogReporter) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, format+"\n", args...)
}

func (l *loopLogReporter) Header(h loop.Header) {
	l.printf("babysitting %s#%d %q", h.Repo, h.Number, h.Title)
	l.printf("branch %s -> %s at %s", h.Branch, h.Base, short(h.HeadSHA))
	l.printf("review model %s, fix model %s",
		engine.Model{Engine: h.ReviewEngine, Name: h.ReviewModel, Effort: h.ReviewEffort}.Tag(),
		engine.Model{Engine: h.Engine, Name: h.Model, Effort: h.Effort}.Tag())
	l.printf("max %d review rounds, %d CI fixes, %s per fix step", h.MaxIter, h.MaxCIFixes, h.FixTimeout)
	l.printf("run dir %s", h.RunDir)
}

func (l *loopLogReporter) Step(title string) { l.printf("==> %s", title) }

func (l *loopLogReporter) StepStart(label string, m engine.Model) {
	l.printf("%s: running on %s", label, m.Tag())
}

// StepTick is dropped: it exists to animate a terminal spinner.
func (l *loopLogReporter) StepTick(string, engine.Model, time.Duration) {}

func (l *loopLogReporter) StepEnd(label string, m engine.Model, elapsed time.Duration, ok bool) {
	status := "failed"
	if ok {
		status = "done"
	}
	l.printf("%s: %s on %s in %s", label, status, m.Tag(), elapsed.Round(time.Second))
}

func (l *loopLogReporter) RoundResult(round int, f review.Findings, clean bool) {
	verdict := "findings remain"
	if clean {
		verdict = "clean"
	}
	l.printf("review round %d: %s (%s)", round, f.Summary(), verdict)
}

// CIWait logs only the start of a wait: the per-second ticks exist to animate
// a terminal status line and would fill a log with identical lines.
func (l *loopLogReporter) CIWait(pr int, elapsed time.Duration) {
	if elapsed == 0 {
		l.printf("waiting for CI on PR #%d...", pr)
	}
}

func (l *loopLogReporter) CIGreen(elapsed time.Duration) {
	l.printf("CI green after %s", elapsed.Round(time.Second))
}
func (l *loopLogReporter) CIRed(attempt, max int) {
	l.printf("CI red; fix attempt %d/%d", attempt, max)
}
func (l *loopLogReporter) Info(s string)             { l.printf("%s", s) }
func (l *loopLogReporter) Warn(s string)             { l.printf("warning: %s", s) }
func (l *loopLogReporter) Questions(text string)     { l.printf("open questions:\n%s", text) }
func (l *loopLogReporter) Dispute(text string)       { l.printf("disputed findings:\n%s", text) }
func (l *loopLogReporter) EnvrcChanged(diff string)  { l.printf(".envrc changed:\n%s", diff) }
func (l *loopLogReporter) Prompt(text string)        { l.printf("%s", text) }
func (l *loopLogReporter) Notify(title, body string) { l.printf("notify: %s - %s", title, body) }

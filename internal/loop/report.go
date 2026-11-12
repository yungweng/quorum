package loop

import (
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/review"
)

// Header is what a pipeline run announces about itself before it starts.
type Header struct {
	Repo       string
	Number     int
	Title      string
	URL        string
	Branch     string
	Base       string
	BranchOnly bool
	Draft      bool
	Local      bool
	// Fix is the fix model as configured. Review carries the resolved review
	// model and effort beside the configured review engine.
	Fix         spec.Model
	Review      spec.Model
	Bypass      bool
	Interactive bool
	Offline     bool
	TestCmd     string
	// TestCmdNote names where a repo-provided test command came from, e.g.
	// ".quorum/testcmd @ main". Empty for a flag or user-local command.
	TestCmdNote    string
	MaxIter        int
	MaxCIFixes     int
	DivergenceScan bool
	FixTimeout     time.Duration
	RunDir         string
	HeadSHA        string
}

// Reporter receives everything a pipeline run wants to say.
//
// The split between StepStart/StepTick/StepEnd exists because a fix round can
// run for an hour with nothing to show: the terminal gets a spinner with the
// step name and elapsed time, and the full session output goes to the run
// directory's logs.
//
// Every step names the model it runs on. A run has two of them, and which one a
// step uses is not derivable from its label: the divergence report and the final
// PR description run on the review model, not the fix model.
type Reporter interface {
	Header(Header)
	Step(title string)
	StepStart(label string, m spec.Model)
	StepTick(label string, m spec.Model, elapsed time.Duration)
	StepEnd(label string, m spec.Model, elapsed time.Duration, ok bool)
	// Activity reports blocking work that does not run on a model. The first
	// call carries elapsed zero; later calls update the same transient line.
	Activity(label string, elapsed time.Duration)
	ActivityDone()
	RoundResult(round int, f review.Findings, clean bool)
	// CIWait is called once when a CI wait starts, with elapsed zero, and then
	// about once a second while it lasts. CIGreen carries how long that wait
	// took, which is often half a run's wall clock and otherwise invisible.
	// review describes the review running beside the wait, such as "review
	// round 1 posted"; it is empty when none does.
	CIWait(pr int, elapsed time.Duration, review string)
	CIGreen(elapsed time.Duration)
	CIRed(attempt, max int)
	Info(string)
	Warn(string)
	// Questions and Dispute surface a session's markers verbatim.
	Questions(text string)
	Dispute(text string)
	EnvrcChanged(diff string)
	// Prompt asks the user something at an interactive gate.
	Prompt(text string)
	// Notify fires a terminal notification.
	Notify(title, body string)
}

type nopReporter struct{}

func (nopReporter) Header(Header)                                   {}
func (nopReporter) Step(string)                                     {}
func (nopReporter) StepStart(string, spec.Model)                    {}
func (nopReporter) StepTick(string, spec.Model, time.Duration)      {}
func (nopReporter) StepEnd(string, spec.Model, time.Duration, bool) {}
func (nopReporter) Activity(string, time.Duration)                  {}
func (nopReporter) ActivityDone()                                   {}
func (nopReporter) RoundResult(int, review.Findings, bool)          {}
func (nopReporter) CIWait(int, time.Duration, string)               {}
func (nopReporter) CIGreen(time.Duration)                           {}
func (nopReporter) CIRed(int, int)                                  {}
func (nopReporter) Info(string)                                     {}
func (nopReporter) Warn(string)                                     {}
func (nopReporter) Questions(string)                                {}
func (nopReporter) Dispute(string)                                  {}
func (nopReporter) EnvrcChanged(string)                             {}
func (nopReporter) Prompt(string)                                   {}
func (nopReporter) Notify(string, string)                           {}

func (r *run) waitActivity(label string, work func() error) error {
	defer r.rep.ActivityDone()
	return r.await(func(elapsed time.Duration) {
		r.rep.Activity(label, elapsed)
	}, work)
}

// await keeps progress moving while work blocks: tick runs at once with zero
// elapsed time, then on every progress tick until work returns. Every
// production caller passes work that observes the run's context; after
// cancellation await joins it so a timed out subprocess cannot keep touching
// the worktree after the pipeline returns.
func (r *run) await(tick func(time.Duration), work func() error) error {
	started := time.Now()
	tick(0)
	done := make(chan error, 1)
	go func() { done <- work() }()

	ticker := time.NewTicker(r.timing.progressTick())
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			tick(time.Since(started))
		case <-r.ctx.Done():
			return <-done
		}
	}
}

// awaitStep is await for a reported step: label's status line ticks with the
// time since started.
func (r *run) awaitStep(label string, m spec.Model, started time.Time, work func() error) error {
	return r.await(func(elapsed time.Duration) {
		if elapsed > 0 {
			r.rep.StepTick(label, m, time.Since(started))
		}
	}, work)
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// CIWaitNote is the suffix a permanent "waiting for CI" line carries for the
// review running beside the wait; empty when none does.
func CIWaitNote(review string) string {
	if review == "" {
		return ""
	}
	return " (" + review + ")"
}

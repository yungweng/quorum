package loop

import (
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/review"
)

// Header is what a pipeline run announces about itself before it starts.
type Header struct {
	Repo           string
	Number         int
	Title          string
	Branch         string
	Base           string
	BranchOnly     bool
	Model          string
	Effort         string
	Bypass         bool
	Interactive    bool
	MaxIter        int
	MaxCIFixes     int
	DivergenceScan bool
	FixTimeout     time.Duration
	RunDir         string
	Worktree       string
	HeadSHA        string
}

// Reporter receives everything a pipeline run wants to say.
//
// The split between StepStart/StepTick/StepEnd exists because a fix round can
// run for an hour with nothing to show: the terminal gets a spinner with the
// step name and elapsed time, and the full Codex output goes to the run
// directory's logs.
type Reporter interface {
	Header(Header)
	Step(title string)
	StepStart(label string)
	StepTick(label string, elapsed time.Duration)
	StepEnd(label string, elapsed time.Duration, ok bool)
	RoundResult(round int, f review.Findings, clean bool)
	CIGreen()
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

// NopReporter discards everything.
type NopReporter struct{}

func (NopReporter) Header(Header)                          {}
func (NopReporter) Step(string)                            {}
func (NopReporter) StepStart(string)                       {}
func (NopReporter) StepTick(string, time.Duration)         {}
func (NopReporter) StepEnd(string, time.Duration, bool)    {}
func (NopReporter) RoundResult(int, review.Findings, bool) {}
func (NopReporter) CIGreen()                               {}
func (NopReporter) CIRed(int, int)                         {}
func (NopReporter) Info(string)                            {}
func (NopReporter) Warn(string)                            {}
func (NopReporter) Questions(string)                       {}
func (NopReporter) Dispute(string)                         {}
func (NopReporter) EnvrcChanged(string)                    {}
func (NopReporter) Prompt(string)                          {}
func (NopReporter) Notify(string, string)                  {}

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

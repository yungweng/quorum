package review

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The lines of a run's event log. A reviewer outcome is its word followed by
// three numbers: "ok idx elapsed rank" and "fail idx exit elapsed". The phase
// lines stand alone.
const (
	eventStart     = "start"
	eventOK        = "ok"
	eventFail      = "fail"
	PhaseAggregate = "aggregate"
	PhaseVerify    = "verify"
)

// appendEvent adds one line to the run's event log.
//
// The log is how a separate process watches a review in flight: the daemon's
// dashboard counts these lines to show "4/6 reviewers done" for a run it did
// not start and cannot call back into. Its existence is itself the signal that
// the reviewers have started, which is why runReviewers writes a "start" line
// before any of them can finish. Outcome lines are appended under the reviewer
// mutex and the start line is written before the reviewers exist, so the
// appends are already serialised.
func appendEvent(outputDir, line string) {
	f, err := os.OpenFile(filepath.Join(outputDir, "events.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// Progress is what a running review reports about itself.
type Progress struct {
	Done      int
	Failed    int
	Requested int
	// Phase is PhaseAggregate or PhaseVerify once the run got that far, and
	// empty while the reviewers run.
	Phase string
}

// ReadProgress counts finished reviewers of a run in flight, from the events
// log the run writes as it goes. A run directory that is not there yet simply
// reports nothing.
//
// The run opens the log with a "start" line when it launches the reviewers, so
// a zero count means the reviewers are running and none has finished yet,
// rather than the run still preparing its worktree. Only reported outcomes are
// counted; any other line is ignored.
func ReadProgress(runDir string, requested int) (Progress, bool) {
	if runDir == "" {
		return Progress{}, false
	}
	f, err := os.Open(filepath.Join(runDir, outputSubdir, "events.log"))
	if err != nil {
		return Progress{}, false
	}
	defer f.Close()
	p := Progress{Requested: requested}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		switch line := sc.Text(); {
		case strings.HasPrefix(line, eventOK+" "):
			p.Done++
		case strings.HasPrefix(line, eventFail+" "):
			p.Failed++
		case line == PhaseAggregate, line == PhaseVerify:
			p.Phase = line
		}
	}
	return p, true
}

package history

import (
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/state"
)

// recorder drives Record against a private state file and history log, the
// way the agent does on every write.
type recorder struct {
	t                      *testing.T
	statePath, historyPath string
}

func newRecorder(t *testing.T) recorder {
	dir := t.TempDir()
	return recorder{t: t, statePath: filepath.Join(dir, "state.json"), historyPath: filepath.Join(dir, "history.jsonl")}
}

// apply runs one state write for acme/api#42 and reports the run it added to
// the history log, if any.
func (r recorder) apply(fn func(*state.Record)) (Run, bool) {
	r.t.Helper()
	before := len(Read(r.historyPath))
	stateErr, historyErr := Record(r.statePath, r.historyPath, "acme/api#42", SourceAgent, fn)
	if stateErr != nil || historyErr != nil {
		r.t.Fatalf("Record: state %v, history %v", stateErr, historyErr)
	}
	runs := Read(r.historyPath)
	if len(runs) == before {
		return Run{}, false
	}
	if len(runs) != before+1 {
		r.t.Fatalf("one write added %d runs", len(runs)-before)
	}
	return runs[0], true
}

// set replaces the record with rec.
func set(rec state.Record) func(*state.Record) {
	return func(r *state.Record) { *r = rec }
}

// Two reviews of the same pull request have to stay two entries. The state
// file collapsing them is the whole reason this log exists.
func TestEveryFinishedReviewIsRecorded(t *testing.T) {
	r := newRecorder(t)
	if _, ok := r.apply(set(state.Record{Status: state.OK, Blockers: 1, At: "2026-01-01T10:00:00Z"})); !ok {
		t.Fatal("the first finished review was not recorded")
	}
	run, ok := r.apply(set(state.Record{Status: state.OK, Title: "a title", Author: "example-user", At: "2026-01-02T10:00:00Z"}))
	if !ok {
		t.Fatal("a second finished review was not recorded")
	}
	if run.Outcome != OK || run.Key != "acme/api#42" || run.Author != "example-user" || !run.Reviewed || run.Source != SourceAgent {
		t.Errorf("run = %+v", run)
	}
	if got := len(Read(r.historyPath)); got != 2 {
		t.Errorf("history holds %d runs, want 2", got)
	}
	f, err := state.Read(r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if f.PRs["acme/api#42"].Author != "example-user" {
		t.Error("the state write was not applied")
	}
}

// Every poll after a review writes the timeline cache back onto the finished
// record, because posting the review moved the pull request's updatedAt. That
// write leaves At alone, so it is the same run, not a new one.
func TestBookkeepingOnAFinishedRecordIsNotANewRun(t *testing.T) {
	r := newRecorder(t)
	r.apply(set(state.Record{Status: state.Running}))
	if _, ok := r.apply(set(state.Record{Status: state.OK, At: "2026-01-01T10:00:00Z", CommentURL: "https://example.invalid/c/1"})); !ok {
		t.Fatal("the finished review was not recorded")
	}
	if _, ok := r.apply(func(rec *state.Record) {
		rec.TimelineAt = "2026-01-01T00:00:00Z"
		rec.SeenReqAt = "2026-01-01T00:00:00Z"
	}); ok {
		t.Error("a timeline lookup was recorded as a second run")
	}
	// The next real run stamps At again, and is a new entry.
	if _, ok := r.apply(set(state.Record{Status: state.OK, At: "2026-01-02T10:00:00Z"})); !ok {
		t.Error("the next review was dropped")
	}
}

// A failure repeated by orphan recovery is the same story: only the write that
// moves At is a run.
func TestARepeatedFailureIsRecordedOnce(t *testing.T) {
	r := newRecorder(t)
	if _, ok := r.apply(set(state.Record{Status: state.Failed, Reason: "codex exited 1", At: "2026-01-01T10:00:00Z"})); !ok {
		t.Fatal("the failure was not recorded")
	}
	if _, ok := r.apply(func(rec *state.Record) { rec.Title = "a title" }); ok {
		t.Error("touching a failed record was recorded as a second failure")
	}
}

// The poll re-applies a skip on every pass. Recording each one would fill the
// log with the same line for as long as the pull request stays open.
func TestARepeatedSkipIsRecordedOnce(t *testing.T) {
	r := newRecorder(t)
	skip := func(reason string) func(*state.Record) {
		return func(rec *state.Record) { rec.Mark(state.Skipped, reason) }
	}
	if _, ok := r.apply(skip("draft")); !ok {
		t.Fatal("the first skip was not recorded")
	}
	if _, ok := r.apply(skip("draft")); ok {
		t.Error("the same skip was recorded twice")
	}
	// A skip for a different reason is a new decision.
	if _, ok := r.apply(skip("opened by a bot")); !ok {
		t.Error("a skip for a new reason was dropped")
	}
}

// Stages of a run in progress belong on the dashboard, not in the log.
func TestUnfinishedStatesAreNotRecorded(t *testing.T) {
	for _, status := range []string{state.Running, state.Pending, state.Deferred, ""} {
		r := newRecorder(t)
		if _, ok := r.apply(set(state.Record{Status: status, At: state.Now()})); ok {
			t.Errorf("status %q reached the history log", status)
		}
	}
}

// A failed review must not carry counts: nothing counted them.
func TestAFailureCarriesNoCounts(t *testing.T) {
	r := newRecorder(t)
	run, ok := r.apply(set(state.Record{Status: state.Failed, Reason: "codex exited 1", Blockers: 3, At: state.Now()}))
	if !ok {
		t.Fatal("a failure was not recorded")
	}
	if run.Reviewed || run.Blockers != 0 {
		t.Errorf("a failure carried review counts: %+v", run)
	}
	if run.Reason != "codex exited 1" || run.Outcome != Failed {
		t.Errorf("run = %+v", run)
	}
}

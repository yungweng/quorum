package history

import (
	"testing"

	"github.com/yungweng/quorum/internal/state"
)

// Two reviews of the same pull request have to stay two entries. The state
// file collapsing them is the whole reason this log exists.
func TestEveryFinishedReviewIsRecorded(t *testing.T) {
	before := state.Record{Status: state.OK, Blockers: 1}
	after := state.Record{Status: state.OK, Title: "a title", At: state.Now()}
	run, ok := FromState("acme/api#42", before, after, SourceAgent)
	if !ok {
		t.Fatal("a second finished review was not recorded")
	}
	if run.Outcome != OK || run.Key != "acme/api#42" || !run.Reviewed {
		t.Errorf("run = %+v", run)
	}
}

// The poll re-applies a skip on every pass. Recording each one would fill the
// log with the same line for as long as the pull request stays open.
func TestARepeatedSkipIsRecordedOnce(t *testing.T) {
	fresh := state.Record{}
	skipped := state.Record{Status: state.Skipped, Reason: "draft", At: state.Now()}
	if _, ok := FromState("acme/api#42", fresh, skipped, SourceAgent); !ok {
		t.Fatal("the first skip was not recorded")
	}
	if _, ok := FromState("acme/api#42", skipped, skipped, SourceAgent); ok {
		t.Error("the same skip was recorded twice")
	}
	// A skip for a different reason is a new decision.
	other := state.Record{Status: state.Skipped, Reason: "opened by a bot", At: state.Now()}
	if _, ok := FromState("acme/api#42", skipped, other, SourceAgent); !ok {
		t.Error("a skip for a new reason was dropped")
	}
}

// Stages of a run in progress belong on the dashboard, not in the log.
func TestUnfinishedStatesAreNotRecorded(t *testing.T) {
	for _, status := range []string{state.Running, state.Pending, state.Deferred, ""} {
		after := state.Record{Status: status, At: state.Now()}
		if _, ok := FromState("acme/api#42", state.Record{}, after, SourceAgent); ok {
			t.Errorf("status %q reached the history log", status)
		}
	}
}

// A failed review must not carry counts: nothing counted them.
func TestAFailureCarriesNoCounts(t *testing.T) {
	after := state.Record{Status: state.Failed, Reason: "codex exited 1", Blockers: 3, At: state.Now()}
	run, ok := FromState("acme/api#42", state.Record{}, after, SourceAgent)
	if !ok {
		t.Fatal("a failure was not recorded")
	}
	if run.Reviewed || run.Blockers != 0 {
		t.Errorf("a failure carried review counts: %+v", run)
	}
	if run.Reason != "codex exited 1" {
		t.Errorf("reason = %q", run.Reason)
	}
}

package cli

import (
	"context"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/state"
)

// The state file keeps its newest records. A skip that was stamped again on
// every poll therefore always counted as new, and enough permanently skipped
// pull requests pushed the finished reviews out.
func TestAStandingSkipKeepsItsTimestamp(t *testing.T) {
	a := testApp(t)
	a.cfg.SkipDrafts = true
	const key, at = "acme/api#42", "2026-08-05T13:00:00Z"
	record(t, a, key, func(r *state.Record) {
		r.Status, r.Reason, r.At = state.Skipped, "draft", at
	})

	var pr gh.PR
	pr.Number, pr.IsDraft = 42, true
	pr.Repository.NameWithOwner = "acme/api"
	if _, ready := a.classify(context.Background(), nil, pr, "reviewer", nil); ready {
		t.Fatal("a draft was queued")
	}

	file, err := state.Read(a.p.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.PRs[key].At; got != at {
		t.Errorf("the standing skip was stamped again: At = %s, want %s", got, at)
	}
	if runs := history.Read(a.p.HistoryFile); len(runs) != 0 {
		t.Errorf("the standing skip was logged again: %+v", runs)
	}

	// A skip for a new reason is a new decision and is recorded as one.
	a.cfg.ExcludeRepos = []string{"acme/api"}
	a.classify(context.Background(), nil, pr, "reviewer", nil)
	file, _ = state.Read(a.p.StateFile)
	if got := file.PRs[key]; got.Reason != "excluded by config" || got.At == at {
		t.Errorf("a new skip reason was not recorded: %+v", got)
	}
}

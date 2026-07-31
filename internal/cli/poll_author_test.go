package cli

import (
	"context"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/state"
)

func TestClassifyRefreshesAuthorWithoutCreatingAHistoryRun(t *testing.T) {
	a := testApp(t)
	const (
		key       = "acme/api#42"
		updatedAt = "2026-07-31T10:00:00Z"
		reqAt     = "2026-07-31T09:00:00Z"
	)
	record(t, a, key, func(r *state.Record) {
		r.Title = "old title"
		r.Status = state.OK
		r.At = state.Now()
		r.ReqAt = reqAt
		r.SeenReqAt = reqAt
		r.TimelineAt = updatedAt
	})

	var pr gh.PR
	pr.Number = 42
	pr.Title = "new title"
	pr.UpdatedAt = updatedAt
	pr.Repository.NameWithOwner = "acme/api"
	pr.Author.Login = "example-user"

	if _, ready := a.classify(context.Background(), nil, pr, "reviewer", nil); ready {
		t.Fatal("an already reviewed request was queued again")
	}
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	got := file.PRs[key]
	if got.Title != "new title" || got.Author != "example-user" {
		t.Errorf("stored metadata = title %q, author %q", got.Title, got.Author)
	}
	if runs := history.Read(a.p.HistoryFile, 0); len(runs) != 0 {
		t.Errorf("metadata refresh created history: %+v", runs)
	}
}

package cli

import (
	"context"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/state"
)

// A give-up binds to the request it gave up on. A newer review request must
// get a fresh retry budget: the old failure count may describe a long-gone
// problem, such as the reviews that died on an exhausted usage limit.
func TestClassifyGivesAFreshBudgetToANewRequestAfterGivingUp(t *testing.T) {
	a := testApp(t)
	const (
		key       = "acme/api#42"
		updatedAt = "2026-08-05T14:00:00Z"
		gaveUpAt  = "2026-08-05T13:00:00Z"
		newReqAt  = "2026-08-05T14:00:00Z"
	)
	record(t, a, key, func(r *state.Record) {
		r.Status = state.GaveUp
		r.Reason = "too many failed attempts"
		r.Fails = 3
		r.ReqAt = gaveUpAt
		r.SeenReqAt = newReqAt
		r.TimelineAt = updatedAt
	})

	var pr gh.PR
	pr.Number = 42
	pr.UpdatedAt = updatedAt
	pr.Repository.NameWithOwner = "acme/api"

	if _, ready := a.classify(context.Background(), nil, pr, "reviewer", nil); !ready {
		t.Fatal("a new request after giving up was not queued")
	}
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := file.PRs[key]; got.Fails != 0 {
		t.Errorf("Fails = %d, want 0 (fresh budget)", got.Fails)
	}
}

// The given-up request itself stays quiet: only a NEWER request timestamp
// reopens the budget, otherwise the give-up would not stick at all.
func TestClassifyKeepsTheGivenUpRequestQuiet(t *testing.T) {
	a := testApp(t)
	const (
		key       = "acme/api#42"
		updatedAt = "2026-08-05T14:00:00Z"
		reqAt     = "2026-08-05T13:00:00Z"
	)
	record(t, a, key, func(r *state.Record) {
		r.Status = state.GaveUp
		r.Fails = 3
		r.ReqAt = reqAt
		r.SeenReqAt = reqAt
		r.TimelineAt = updatedAt
	})

	var pr gh.PR
	pr.Number = 42
	pr.UpdatedAt = updatedAt
	pr.Repository.NameWithOwner = "acme/api"

	if _, ready := a.classify(context.Background(), nil, pr, "reviewer", nil); ready {
		t.Fatal("the request that was given up on was queued again")
	}
}

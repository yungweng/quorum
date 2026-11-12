package runner

import (
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/state"
)

func TestSummaryLine(t *testing.T) {
	if got := summaryLine(review.Findings{}); got != "nothing found" {
		t.Errorf("empty findings rendered as %q", got)
	}
	if got := summaryLine(review.Findings{Critical: 4}); got != "0 blockers, 4 critical, 0 suggestions" {
		t.Errorf("got %q", got)
	}
	// A run that only raised questions still has something to report.
	if got := summaryLine(review.Findings{Questions: 2}); got == "nothing found" {
		t.Error("questions were reported as nothing found")
	}
}

func TestNotifyUsesTerminalForForegroundRun(t *testing.T) {
	var gotTitle, gotBody string
	r := &Runner{
		Cfg: config.Config{Notify: true},
		TerminalNotify: func(title, body string) {
			gotTitle, gotBody = title, body
		},
	}

	r.notify("Reviewed api#42", "nothing found", "https://example.invalid/comment/42")

	if gotTitle != "Reviewed api#42" || gotBody != "nothing found" {
		t.Fatalf("terminal notification = %q / %q", gotTitle, gotBody)
	}
}

func TestNotifySettingDisablesTerminalNotification(t *testing.T) {
	called := false
	r := &Runner{TerminalNotify: func(string, string) { called = true }}

	r.notify("Reviewed api#42", "nothing found", "")

	if called {
		t.Fatal("notification sent although NOTIFY is disabled")
	}
}

func TestAutoMergeFailurePreservesSuccessfulReview(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{P: paths.P{
		StateFile:   filepath.Join(dir, "state.json"),
		HistoryFile: filepath.Join(dir, "history.jsonl"),
	}}
	if err := state.Mutate(r.P.StateFile, "acme/api#42", func(rec *state.Record) {
		rec.Fails = 2
	}); err != nil {
		t.Fatal(err)
	}
	url := "https://example.invalid/comment/42"
	reason := "auto-merge failed after the approval was posted"
	r.recordAutoMergeFailure("acme/api#42", "2026-07-31T09:00:00Z", "/run/42", review.Findings{
		PR: 42, HeadSHA: "abc123", Posted: true, CommentURL: &url, Suggestions: 2, Questions: 1,
	}, reason)

	file, err := state.Read(r.P.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := file.PRs["acme/api#42"]
	if rec.Status != state.OK || rec.Reason != reason || rec.ReqAt != "2026-07-31T09:00:00Z" || rec.SHA != "abc123" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.CommentURL != url || rec.Suggestions != 2 || rec.Questions != 1 || rec.Fails != 0 {
		t.Fatalf("review result was not preserved: %+v", rec)
	}
	runs := history.Read(r.P.HistoryFile)
	if len(runs) != 1 || runs[0].Outcome != history.OK || !runs[0].Reviewed || runs[0].Reason != reason {
		t.Fatalf("history = %+v", runs)
	}
}

func TestDivergenceResultStopsAgentRetries(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{P: paths.P{
		StateFile:   filepath.Join(dir, "state.json"),
		HistoryFile: filepath.Join(dir, "history.jsonl"),
	}}
	if err := state.Mutate(r.P.StateFile, "acme/api#42", func(rec *state.Record) {
		rec.Fails = 2
	}); err != nil {
		t.Fatal(err)
	}
	reviewURL := "https://example.invalid/review"
	r.recordDivergenceResult(
		"acme/api#42", "abc123", "2026-07-31T09:00:00Z", "/run/42", "/log/42",
		"https://example.invalid/divergence",
		review.Findings{Critical: 1, CommentURL: &reviewURL},
		"review loop diverged after 12 review rounds",
	)

	file, err := state.Read(r.P.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := file.PRs["acme/api#42"]
	if rec.Status != state.Failed || rec.ReqAt != "2026-07-31T09:00:00Z" || rec.Fails != 0 {
		t.Fatalf("divergence did not stop retries: %+v", rec)
	}
	if rec.CommentURL != "https://example.invalid/divergence" || rec.Critical != 1 ||
		rec.RunDir != "/run/42" || rec.RunLog != "/log/42" {
		t.Fatalf("divergence result was not preserved: %+v", rec)
	}
}

func TestAutoMergePendingPreservesReviewWithoutHandlingRequest(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{P: paths.P{
		StateFile:   filepath.Join(dir, "state.json"),
		HistoryFile: filepath.Join(dir, "history.jsonl"),
	}}
	r.recordAutoMergePending("acme/api#42", "/run/42", review.Findings{
		PR: 42, HeadSHA: "abc123", Posted: true, Suggestions: 2,
	})

	file, err := state.Read(r.P.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := file.PRs["acme/api#42"]
	if rec.Status != state.Running || rec.ReqAt != "" || rec.SHA != "abc123" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Reason != "waiting for required checks before merge" || rec.Suggestions != 2 {
		t.Fatalf("review result was not preserved: %+v", rec)
	}
}

// `quorum run` has no review request of its own. Its empty timestamp used to
// be stored as the handled one, and the next poll reviewed the same pull
// request again for a request the agent had long answered.
func TestAManualRunDoesNotForgetTheHandledRequest(t *testing.T) {
	const key, handled, newer = "acme/api#42", "2026-08-05T13:00:00Z", "2026-08-05T15:00:00Z"
	r := &Runner{P: paths.P{StateFile: filepath.Join(t.TempDir(), "state.json")}}
	if got := r.requestAnswered(key, ""); got != "" {
		t.Errorf("a manual run before any poll answers %q, want no request", got)
	}
	if err := state.Mutate(r.P.StateFile, key, func(rec *state.Record) {
		rec.ReqAt, rec.SeenReqAt = handled, newer
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.requestAnswered(key, handled); got != handled {
		t.Errorf("the poll's request was replaced by %q", got)
	}
	// The newest request seen when the run begins, read once: one that
	// arrives while the run is going has not been reviewed by it.
	if got := r.requestAnswered(key, ""); got != newer {
		t.Errorf("a manual run answers %q, want the newest request seen, %q", got, newer)
	}

	rec := state.Record{ReqAt: handled, SeenReqAt: newer}
	markHandled(&rec, "")
	if rec.ReqAt != handled {
		t.Errorf("a run without a request recorded %q over the handled %q", rec.ReqAt, handled)
	}
	markHandled(&rec, newer)
	if rec.ReqAt != newer {
		t.Errorf("ReqAt = %q, want %q", rec.ReqAt, newer)
	}
}

// REVIEWERS is checked when the config is read, against the review core's own
// limit. The two must not drift.
func TestConfigAndReviewAgreeOnTheReviewerLimit(t *testing.T) {
	if config.MaxReviewers != review.MaxRuns {
		t.Fatalf("config.MaxReviewers = %d, review.MaxRuns = %d", config.MaxReviewers, review.MaxRuns)
	}
}

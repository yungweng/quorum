package automerge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
)

func fakeGH(t *testing.T, cases string) (*gh.Client, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	count := filepath.Join(dir, "count")
	args := filepath.Join(dir, "args")
	script := "#!/bin/bash\n" +
		"n=$(cat " + count + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1)); echo $n > " + count + "\n" +
		"printf '%s\\n' \"$*\" >> " + args + "\n" +
		cases + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	client := gh.New(bin)
	client.Backoff = time.Millisecond
	client.Timeout = 5 * time.Second
	return client, args
}

func readArgs(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunApprovesAndMergesExactHead(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  7) echo '{"merged":true}' ;;
esac`)

	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ApprovalCreated || result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	for _, want := range []string{
		"event=APPROVE",
		"commit_id=abc123",
		"body=" + approvalBody,
		"api --method PUT repos/acme/api/pulls/42/merge -f merge_method=merge -f sha=abc123",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("calls are missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--auto") {
		t.Fatalf("merge created a persistent request:\n%s", args)
	}
}

func TestRunReusesApprovalAndMerges(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"id":77,"state":"APPROVED","commit_id":"abc123","body":"`+approvalText+`","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  5) echo '{"merged":true}' ;;
esac`)

	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result.ApprovalCreated || result.approvalReviewID != 0 || result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") {
		t.Fatalf("an idempotent rerun repeated the approval:\n%s", args)
	}
}

func TestRunRejectsActiveChangeRequests(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","reviewDecision":"CHANGES_REQUESTED","author":{"login":"example-user"}}' ;;
esac`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "active change requests") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("active change request reached a side effect:\n%s", args)
	}
}

func TestRunRejectsUnprotectedExternalChangeRequest(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"id":10,"state":"CHANGES_REQUESTED","commit_id":"abc123","submitted_at":"2026-07-31T10:00:00Z","user":{"login":"other-reviewer"}},{"id":11,"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T10:01:00Z","user":{"login":"reviewer"}}]' ;;
esac`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "active change requests") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("external change request reached a side effect:\n%s", args)
	}
}

func TestRunAllowsSupersededExternalChangeRequest(t *testing.T) {
	client, _ := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"id":10,"state":"CHANGES_REQUESTED","commit_id":"abc123","submitted_at":"2026-07-31T10:00:00Z","user":{"login":"other-reviewer"}},{"id":11,"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T10:01:00Z","user":{"login":"other-reviewer"}},{"id":12,"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T10:02:00Z","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"merged":true}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunRejectsMergeQueueBeforeApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"baseRefName":"main","headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo '{"data":{"repository":{"mergeCommitAllowed":true,"pullRequest":{"headRefOid":"abc123","isMergeQueueEnabled":true}}}}' ;;
esac`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "requires a merge queue") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("merge queue branch reached a side effect:\n%s", args)
	}
}

func TestRunRejectsDisabledMergeCommitsBeforeApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"baseRefName":"main","headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo '{"data":{"repository":{"mergeCommitAllowed":false,"pullRequest":{"headRefOid":"abc123","isMergeQueueEnabled":false}}}}' ;;
esac`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "does not allow merge commits") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("disabled merge method reached a side effect:\n%s", args)
	}
}

func TestRunRejectsDraftBeforeApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","isDraft":true,"author":{"login":"example-user"}}' ;;
esac`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "is a draft") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("draft reached a side effect:\n%s", args)
	}
}

func TestRunRejectsChangeRequestCreatedAfterApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","latestReviews":[{"state":"CHANGES_REQUESTED","author":{"login":"other-reviewer"}}],"author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "active change requests") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated || result.approvalReviewID != 0 {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	if !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("created approval was not dismissed:\n%s", args)
	}
	if strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("late change request reached merge:\n%s", args)
	}
}

func TestRunTracksReusedAutomaticApprovalForCleanup(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"id":99,"state":"APPROVED","commit_id":"abc123","body":"`+approvalBody+`","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo 'Pull Request is not mergeable (HTTP 405)' >&2; exit 1 ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","author":{"login":"example-user"}}' ;;
  7) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  8) echo 'all checks passed' ;;
  9) echo '{"headRefOid":"new-head","state":"OPEN","author":{"login":"example-user"}}' ;;
esac`)

	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if !errors.Is(err, ErrMergeNotReady) {
		t.Fatalf("err = %v, want ErrMergeNotReady", err)
	}
	if result.ApprovalCreated || result.approvalReviewID != 99 {
		t.Fatalf("reused automatic approval was not tracked: %+v", result)
	}

	result, err = RetryWhenReady(context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", result, time.Second)
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if result.approvalReviewID != 0 {
		t.Fatalf("reused automatic approval was not dismissed: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("reused automatic approval was not dismissed:\n%s", args)
	}
}

func TestRunDoesNotReuseSupersededApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"CHANGES_REQUESTED","commit_id":"abc123","submitted_at":"2026-07-31T10:01:00Z","user":{"login":"reviewer"}},{"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T10:00:00Z","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  7) echo '{"merged":true}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ApprovalCreated {
		t.Fatalf("superseded approval was reused: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "event=APPROVE") {
		t.Fatalf("approval was not renewed:\n%s", args)
	}
}

func TestRunOrdersEqualReviewTimestampsByID(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"id":102,"state":"CHANGES_REQUESTED","commit_id":"abc123","submitted_at":"2026-07-31T10:00:00Z","user":{"login":"reviewer"}},{"id":101,"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T10:00:00Z","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":103,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  7) echo '{"merged":true}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ApprovalCreated {
		t.Fatalf("obsolete approval was treated as the current review: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "event=APPROVE") {
		t.Fatalf("approval was not renewed after the newer change request:\n%s", args)
	}
}

func TestRunReportsPendingBranchRequirements(t *testing.T) {
	client, _ := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo 'Pull Request is not mergeable (HTTP 405)' >&2; exit 1 ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","author":{"login":"example-user"}}' ;;
esac`)

	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if !errors.Is(err, ErrMergeNotReady) {
		t.Fatalf("err = %v, want ErrMergeNotReady", err)
	}
}

func TestRunDismissesApprovalForTerminal405(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  7) echo 'Pull Request is not mergeable (HTTP 405)' >&2; exit 1 ;;
  8) echo '{"headRefOid":"abc123","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || errors.Is(err, ErrMergeNotReady) {
		t.Fatalf("err = %v, want terminal merge failure", err)
	}
	if result.approvalReviewID != 0 {
		t.Fatalf("terminal 405 left approval active: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("terminal 405 did not dismiss approval:\n%s", args)
	}
}

func TestMergeReadinessPendingRequiresUnsettledState(t *testing.T) {
	err405 := errors.New("Pull Request is not mergeable (HTTP 405)")
	methodErr := errors.New("Merge commits are not allowed on this repository (HTTP 405)")
	cases := []struct {
		name      string
		details   gh.Details
		err       error
		retryable bool
	}{
		{"requirements blocked", gh.Details{Mergeable: "MERGEABLE", MergeStateStatus: "BLOCKED"}, err405, true},
		{"mergeability unknown", gh.Details{Mergeable: "UNKNOWN", MergeStateStatus: "UNKNOWN"}, err405, true},
		{"merge method rejected", gh.Details{Mergeable: "MERGEABLE", MergeStateStatus: "BLOCKED"}, methodErr, false},
		{"conflict", gh.Details{Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"}, err405, false},
		{"non-405", gh.Details{Mergeable: "MERGEABLE", MergeStateStatus: "BLOCKED"}, errors.New("forbidden"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeReadinessPending(tc.details, tc.err); got != tc.retryable {
				t.Fatalf("mergeReadinessPending() = %v, want %v", got, tc.retryable)
			}
		})
	}
}

func TestRunRejectsHeadDriftImmediatelyBeforeApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"new-head","state":"OPEN","author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if result.ApprovalAttempted {
		t.Fatalf("moved head reached approval: %+v", result)
	}
	if args := readArgs(t, argsFile); strings.Contains(args, "event=APPROVE") {
		t.Fatalf("moved head reached approval:\n%s", args)
	}
}

func TestRunRejectsHeadDriftBeforeApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `echo '{"headRefOid":"new-head","state":"OPEN","author":{"login":"example-user"}}'`)
	_, err := Run(context.Background(), client, "acme/api", 42, "reviewed-head")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(readArgs(t, argsFile)), "\n") + 1; lines != 1 {
		t.Fatalf("head drift made %d gh calls, want one", lines)
	}
}

func TestRunRejectsForkBeforeApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `echo '{"headRefOid":"abc123","state":"OPEN","isCrossRepository":true,"author":{"login":"example-user"}}'`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "fork pull request") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("fork PR reached a side effect:\n%s", args)
	}
}

func TestRunRejectsMergedDifferentHead(t *testing.T) {
	client, _ := fakeGH(t, `echo '{"headRefOid":"new-head","state":"MERGED","author":{"login":"example-user"}}'`)
	result, err := Run(context.Background(), client, "acme/api", 42, "reviewed-head")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if result.Status == Merged {
		t.Fatalf("a different merged head was accepted: %+v", result)
	}
}

func TestRunRejectsExistingAutoMergeBeforeApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
	echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}'`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "already has an auto-merge request") {
		t.Fatalf("err = %v", err)
	}
	if result.ApprovalAttempted {
		t.Fatalf("existing auto-merge request reached approval: %+v", result)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") || strings.Contains(args, "--disable-auto") {
		t.Fatalf("existing auto-merge request reached a side effect:\n%s", args)
	}
}

func TestRunPreservesExistingAutoMergeOnHeadDrift(t *testing.T) {
	client, argsFile := fakeGH(t, `
	echo '{"headRefOid":"new-head","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}'`)
	_, err := Run(context.Background(), client, "acme/api", 42, "reviewed-head")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if args := readArgs(t, argsFile); strings.Contains(args, "--disable-auto") {
		t.Fatalf("head drift cancelled an existing request:\n%s", args)
	}
}

func TestRunDismissesCreatedApprovalOnHeadDrift(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"new-head","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated {
		t.Fatalf("partial result lost the approval: %+v", result)
	}
	if args := readArgs(t, argsFile); strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("a moved head reached merge:\n%s", args)
	}
	for _, want := range []string{
		"pulls/42/reviews/99/dismissals",
		"message=" + driftDismissalBody,
	} {
		if args := readArgs(t, argsFile); !strings.Contains(args, want) {
			t.Errorf("calls are missing %q:\n%s", want, args)
		}
	}
	if args := readArgs(t, argsFile); strings.Contains(args, "event=DISMISS") {
		t.Fatalf("dismissal sent the unsupported event parameter:\n%s", args)
	}
}

func TestRunReconcilesTimedOutApprovalOnHeadDrift(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  6) echo '[{"id":99,"state":"APPROVED","commit_id":"abc123","body":"`+approvalBody+`","user":{"login":"reviewer"}}]' ;;
  7) echo '{"headRefOid":"new-head","state":"OPEN","author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalAttempted || !result.ApprovalCreated || result.approvalReviewID != 0 {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	if !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("timed-out approval was not dismissed:\n%s", args)
	}
}

func TestRunDismissesReconciledApprovalWhenApprovalFails(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  6) echo '[{"id":99,"state":"APPROVED","commit_id":"abc123","body":"`+approvalBody+`","user":{"login":"reviewer"}}]' ;;
  7) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "approving reviewed head") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated || result.approvalReviewID != 0 {
		t.Fatalf("result = %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("reconciled approval was not dismissed:\n%s", args)
	}
}

func TestRunRejectsOwnPullRequest(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"reviewer"}}' ;;
  2) echo 'reviewer' ;;
esac`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "your own pull request") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("own PR reached a side effect:\n%s", args)
	}
}

func TestRunReportsApprovalWhenMergeFails(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  7) echo '{"merged":false,"message":"Base branch was modified"}' ;;
  8) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "merging reviewed head") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated {
		t.Fatalf("partial result lost the approval: %+v", result)
	}
	if result.Status == Merged {
		t.Fatalf("an unmerged response was reported as merged: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("terminal merge failure left the approval active:\n%s", args)
	}
}

func TestRunDismissesApprovalWhenMergeInspectionFails(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  7) echo 'merge request failed' >&2; exit 1 ;;
  8|9|10) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "rechecking pull request") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated || result.approvalReviewID != 0 {
		t.Fatalf("result = %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("uncertain merge left the approval active:\n%s", args)
	}
}

func TestRunAcceptsMergeAfterAmbiguousCommandFailure(t *testing.T) {
	client, _ := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  5) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  6) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  7) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  8) echo '{"headRefOid":"abc123","state":"MERGED","author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunRejectsDifferentHeadAfterAmbiguousMergeFailure(t *testing.T) {
	client, _ := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5|6|7) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  8) echo '{"headRefOid":"new-head","state":"MERGED","author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if result.Status == Merged {
		t.Fatalf("a different merged head was accepted: %+v", result)
	}
}

func TestRetryDismissesApprovalWhenHeadMovesDuringChecks(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'all checks passed' ;;
  3) echo '{"headRefOid":"new-head","state":"OPEN","author":{"login":"example-user"}}' ;;
esac`)
	result, err := RetryWhenReady(context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", Result{
		ApprovalCreated: true, approvalReviewID: 99,
	}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if result.approvalReviewID != 0 {
		t.Fatalf("created approval was not dismissed: %+v", result)
	}
	args := readArgs(t, argsFile)
	if !strings.Contains(args, "pulls/42/reviews/99/dismissals") || strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("calls =\n%s", args)
	}
}

func TestRetryBoundsChecksWatch(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) while true; do :; done ;;
esac`)
	const waitTimeout = 5 * time.Second
	started := time.Now()
	result, err := RetryWhenReady(context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", Result{
		ApprovalCreated: true, approvalReviewID: 99,
	}, waitTimeout)
	if err == nil || !strings.Contains(err.Error(), "did not settle") {
		t.Fatalf("err = %v", err)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pr checks 42 --watch --fail-fast --required") {
		t.Fatalf("checks watch was not reached:\n%s", args)
	}
	if elapsed := time.Since(started); elapsed > waitTimeout+2*time.Second {
		t.Fatalf("checks watch took %s, want under %s", elapsed, waitTimeout+2*time.Second)
	}
	if result.approvalReviewID != 0 {
		t.Fatalf("timed-out retry left the approval active: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("timed-out retry did not dismiss the approval:\n%s", args)
	}
}

func TestRetryDismissesApprovalWhenChecksFail(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1|3) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'build fail 1m' >&2; exit 1 ;;
esac`)
	result, err := RetryWhenReady(context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", Result{
		ApprovalCreated: true, approvalReviewID: 99,
	}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "required checks failed") {
		t.Fatalf("err = %v", err)
	}
	if result.approvalReviewID != 0 {
		t.Fatalf("failed checks left the approval active: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("failed checks did not dismiss the approval:\n%s", args)
	}
}

func TestDismissApprovalUsesFreshContextAfterCancellation(t *testing.T) {
	client, argsFile := fakeGH(t, ``)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cause := errors.New("auto-merge canceled")
	result := Result{ApprovalCreated: true, approvalReviewID: 99}

	err := dismissCreatedApprovalAfterFailure(ctx, client, "acme/api", 42, &result, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want original cause", err)
	}
	if result.approvalReviewID != 0 {
		t.Fatalf("canceled run left the approval active: %+v", result)
	}
	if args := readArgs(t, argsFile); !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("canceled run did not dismiss the approval:\n%s", args)
	}
}

func TestRetryRetriesTransientChecksWatchFailure(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  3) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  4) echo 'all checks passed' ;;
  5) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  7) echo 'reviewer' ;;
  8) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  9) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  10) echo '{"merged":true}' ;;
esac`)
	result, err := RetryWhenReady(context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", Result{}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	if calls := strings.Count(readArgs(t, argsFile), "pr checks 42 --watch --fail-fast --required"); calls != 2 {
		t.Fatalf("checks watch ran %d times, want two", calls)
	}
}

func TestRetryWithoutChecksAllowsUnlimitedWait(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1|3|4|7) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'no required checks reported on the topic branch' >&2; exit 1 ;;
  5) echo 'reviewer' ;;
  6) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  8) echo '{"merged":true}' ;;
esac`)
	result, err := RetryWhenReady(context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", Result{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	if calls := strings.Count(args, "pr checks 42 --watch --fail-fast --required"); calls != 1 {
		t.Fatalf("required checks watch ran %d times, want one:\n%s", calls, args)
	}
}

func TestRunStopsForAutoMergeRequestCreatedDuringApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "already has an auto-merge request") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated || result.approvalReviewID != 0 {
		t.Fatalf("approval was not cleaned up: %+v", result)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "--disable-auto") {
		t.Fatalf("run cancelled an existing request:\n%s", args)
	}
	if strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("existing request reached direct merge:\n%s", args)
	}
	if !strings.Contains(args, "pulls/42/reviews/99/dismissals") {
		t.Fatalf("created approval was not dismissed:\n%s", args)
	}
}

func TestEligibleAllowsSuggestionsAndQuestionsOnly(t *testing.T) {
	if !Eligible(review.Findings{PR: 42, Posted: true, Suggestions: 3, Questions: 2}) {
		t.Fatal("suggestions or questions blocked auto-merge")
	}
	for _, findings := range []review.Findings{
		{PR: 42, Posted: true, Blockers: 1},
		{PR: 42, Posted: true, Critical: 1},
		{PR: 42, Posted: false},
		{PR: 0, Posted: true},
	} {
		if Eligible(findings) {
			t.Errorf("ineligible findings were accepted: %+v", findings)
		}
	}
}

func TestAllowedRequiresPostingAndSourceOption(t *testing.T) {
	findings := review.Findings{PR: 42, Posted: true}
	if !Allowed(true, true, findings) {
		t.Fatal("posted clean review was rejected")
	}
	if Allowed(true, false, findings) {
		t.Fatal("POST=0 allowed auto-merge")
	}
	if Allowed(false, true, findings) {
		t.Fatal("disabled source allowed auto-merge")
	}
}

package automerge

import (
	"context"
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
  5) echo '{"state":"APPROVED"}' ;;
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
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  5) echo '{"merged":true}' ;;
esac`)

	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result.ApprovalCreated || result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") {
		t.Fatalf("an idempotent rerun repeated the approval:\n%s", args)
	}
}

func TestRunDoesNotReuseSupersededApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}},{"state":"CHANGES_REQUESTED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"state":"APPROVED"}' ;;
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

func TestRunRejectsHeadDriftAfterApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"state":"APPROVED"}' ;;
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
	client, _ := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  7) echo 'branch protection rejected the merge' >&2; exit 1 ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "merging reviewed head") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated {
		t.Fatalf("partial result lost the approval: %+v", result)
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

func TestRunPreservesExistingAutoMergeWhenDirectMergeFails(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
  5) echo 'branch protection rejected the merge' >&2; exit 1 ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
esac`)
	_, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "merging reviewed head") {
		t.Fatalf("err = %v", err)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "--disable-auto") {
		t.Fatalf("failed direct merge cancelled an existing request:\n%s", args)
	}
	if !strings.Contains(args, "pulls/42/merge") {
		t.Fatalf("direct merge was not attempted:\n%s", args)
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

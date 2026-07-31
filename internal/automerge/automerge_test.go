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
	client.Timeout = time.Second
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

func TestRunApprovesExactHeadAndRequestsMerge(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"state":"APPROVED"}' ;;
  5) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  6) exit 0 ;;
  7) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
esac`)

	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !result.ApprovalCreated || result.Status != Requested {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	for _, want := range []string{
		"event=APPROVE",
		"commit_id=abc123",
		"body=" + approvalBody,
		"pr merge 42 --repo acme/api --auto --merge --match-head-commit abc123",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("calls are missing %q:\n%s", want, args)
		}
	}
}

func TestRunReusesApprovalAndExistingAutoMerge(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
esac`)

	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result.ApprovalCreated || result.Status != Requested {
		t.Fatalf("result = %+v", result)
	}
	args := readArgs(t, argsFile)
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pr merge") {
		t.Fatalf("an idempotent rerun repeated a side effect:\n%s", args)
	}
}

func TestRunDoesNotReuseSupersededApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}},{"state":"CHANGES_REQUESTED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"state":"APPROVED"}' ;;
  5) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
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

func TestRunRejectsHeadDriftAfterApproval(t *testing.T) {
	client, argsFile := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"state":"APPROVED"}' ;;
  5) echo '{"headRefOid":"new-head","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "refusing auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated {
		t.Fatalf("partial result lost the approval: %+v", result)
	}
	if args := readArgs(t, argsFile); strings.Contains(args, "pr merge") {
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
	if strings.Contains(args, "event=APPROVE") || strings.Contains(args, "pr merge") {
		t.Fatalf("own PR reached a side effect:\n%s", args)
	}
}

func TestRunReportsApprovalWhenMergeFails(t *testing.T) {
	client, _ := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"state":"APPROVED"}' ;;
  5) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  6) echo 'auto-merge is disabled' >&2; exit 1 ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err == nil || !strings.Contains(err.Error(), "enabling auto-merge") {
		t.Fatalf("err = %v", err)
	}
	if !result.ApprovalCreated {
		t.Fatalf("partial result lost the approval: %+v", result)
	}
}

func TestRunAcceptsMergeRequestAfterAmbiguousCommandFailure(t *testing.T) {
	client, _ := fakeGH(t, `
case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[{"state":"APPROVED","commit_id":"abc123","user":{"login":"reviewer"}}]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":null,"author":{"login":"example-user"}}' ;;
  5) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  6) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  7) echo 'net/http: TLS handshake timeout' >&2; exit 1 ;;
  8) echo '{"headRefOid":"abc123","state":"OPEN","autoMergeRequest":{"enabledAt":"now"},"author":{"login":"example-user"}}' ;;
esac`)
	result, err := Run(context.Background(), client, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Requested {
		t.Fatalf("result = %+v", result)
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

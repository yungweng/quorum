package automerge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/gh"
)

func TestCompleteWaitsForChecksAfterGitHubRefusesTheMerge(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	count := filepath.Join(dir, "count")
	args := filepath.Join(dir, "args")
	script := "#!/bin/bash\n" +
		"n=$(cat " + count + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1)); echo $n > " + count + "\n" +
		"printf '%s\\n' \"$*\" >> " + args + "\n" +
		`case "$n" in
  1) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'reviewer' ;;
  3) echo '[]' ;;
  4) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  5) echo '{"id":99,"state":"APPROVED"}' ;;
  6) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  7) echo 'Pull Request is not mergeable (HTTP 405)' >&2; exit 1 ;;
  8) echo '{"headRefOid":"abc123","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","author":{"login":"example-user"}}' ;;
  9|11|12|15) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  10) echo 'all checks passed' ;;
  13) echo 'reviewer' ;;
  14) echo '[{"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T09:00:00Z","user":{"login":"reviewer"}}]' ;;
  16) echo '{"merged":true}' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	client := gh.New(bin)
	client.Backoff = time.Millisecond
	client.Timeout = 5 * time.Second

	waited := false
	result, err := Complete(context.Background(), client, dir, "acme/api", 42, "abc123", nil, 0, func() { waited = true })
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged || !result.approvalCreated || !waited {
		t.Fatalf("result = %+v, waited = %v", result, waited)
	}
	calls, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "pr checks 42 --watch --fail-fast --required") {
		t.Fatalf("the merge did not wait for checks:\n%s", calls)
	}
}

func TestRetryAutoMergeAfterChecksPass(t *testing.T) {
	client, args := fakeGH(t, `case "$n" in
  1|3|4|7) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'all checks passed' ;;
  5) echo 'reviewer' ;;
  6) echo '[{"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T09:00:00Z","user":{"login":"reviewer"}}]' ;;
  8) echo '{"merged":true}' ;;
esac
`)
	result, err := retryWhenReady(context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", nil, Result{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	calls, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pr checks 42 --watch --fail-fast --required", "pulls/42/merge", "sha=abc123"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("calls are missing %q:\n%s", want, calls)
		}
	}
}

func TestRetryAutoMergeGivesChecksTimeToRegister(t *testing.T) {
	client, args := fakeGH(t, `case "$n" in
  1|3|4|7|10|12|13|16) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2) echo 'no required checks reported on the topic branch' >&2; exit 1 ;;
  5|14) echo 'reviewer' ;;
  6|15) echo '[{"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T09:00:00Z","user":{"login":"reviewer"}}]' ;;
  8) echo 'Pull Request is not mergeable (HTTP 405)' >&2; exit 1 ;;
  9) echo '{"headRefOid":"abc123","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","author":{"login":"example-user"}}' ;;
  11) echo 'all checks passed' ;;
  17) echo '{"merged":true}' ;;
esac`)
	result, err := retryWhenReady(
		context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", nil, Result{}, 10*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	calls, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "pr checks 42 --watch --fail-fast --required"); got != 2 {
		t.Fatalf("check calls = %d, want 2:\n%s", got, calls)
	}
}

func TestRetryAutoMergeWaitsForMergeabilityAfterGreenChecks(t *testing.T) {
	client, args := fakeGH(t, `case "$n" in
  1|3|4|7|10|12|13|16) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  2|11) echo 'all checks passed' ;;
  5|14) echo 'reviewer' ;;
  6|15) echo '[{"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T09:00:00Z","user":{"login":"reviewer"}}]' ;;
  8) echo 'Pull Request is not mergeable (HTTP 405)' >&2; exit 1 ;;
  9) echo '{"headRefOid":"abc123","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","author":{"login":"example-user"}}' ;;
  17) echo '{"merged":true}' ;;
esac`)
	result, err := retryWhenReady(
		context.Background(), client, t.TempDir(), "acme/api", 42, "abc123", nil, Result{}, 10*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != Merged {
		t.Fatalf("result = %+v", result)
	}
	calls, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "pulls/42/merge"); got != 2 {
		t.Fatalf("merge calls = %d, want 2:\n%s", got, calls)
	}
}

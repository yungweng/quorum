package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/automerge"
	"github.com/yungweng/quorum/internal/gh"
)

func TestManualAutoMergeWaitsForChecks(t *testing.T) {
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
  8) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  9) echo 'all checks passed' ;;
  10) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  11) echo 'reviewer' ;;
  12) echo '[{"state":"APPROVED","commit_id":"abc123","submitted_at":"2026-07-31T09:00:00Z","user":{"login":"reviewer"}}]' ;;
  13) echo '{"headRefOid":"abc123","state":"OPEN","author":{"login":"example-user"}}' ;;
  14) echo '{"merged":true}' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	client := gh.New(bin)
	client.Backoff = time.Millisecond
	client.Timeout = 5 * time.Second

	result, err := (&app{}).autoMerge(context.Background(), client, dir, "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != automerge.Merged || !result.ApprovalCreated {
		t.Fatalf("result = %+v", result)
	}
	calls, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "pr checks 42 --watch --fail-fast") {
		t.Fatalf("manual merge did not wait for checks:\n%s", calls)
	}
}

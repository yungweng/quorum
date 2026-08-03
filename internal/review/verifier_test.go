package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/git"
)

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func fakeVerifier(t *testing.T, result string, mutate bool) (runPaths, envexec.Env, codex.Options, git.G, string) {
	t.Helper()
	root := t.TempDir()
	output := filepath.Join(root, "output")
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, worktree, "init", "-q")
	runTestGit(t, worktree, "config", "user.email", "example@example.invalid")
	runTestGit(t, worktree, "config", "user.name", "Example User")
	if err := os.WriteFile(filepath.Join(worktree, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, worktree, "add", "seed.txt")
	runTestGit(t, worktree, "commit", "-q", "-m", "Initial fixture")
	head := runTestGit(t, worktree, "rev-parse", "HEAD")
	candidate := filepath.Join(output, "aggregated-pr-comment.md")
	if err := os.WriteFile(candidate, []byte(goodComment), 0o644); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(root, "verifier-result.md")
	if err := os.WriteFile(resultPath, []byte(result), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUORUM_TEST_VERIFIER_RESULT", resultPath)

	bin := filepath.Join(root, "codex")
	script := `#!/bin/sh
set -eu
case " $* " in
  *" --sandbox workspace-write "*) ;;
  *) echo "workspace-write sandbox missing" >&2; exit 9 ;;
esac
case " $* " in
  *" --dangerously-bypass-approvals-and-sandbox "*) echo "sandbox bypass present" >&2; exit 10 ;;
esac
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    out="$2"
    shift 2
    continue
  fi
  shift
done
test -n "$out"
cp "$QUORUM_TEST_VERIFIER_RESULT" "$out"
`
	if mutate {
		script += "printf 'changed\\n' > verifier-mutated.txt\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runPaths{
		output: output, worktree: worktree, candidate: candidate,
		comment: filepath.Join(output, "final-pr-comment.md"),
		changes: filepath.Join(output, "verification-changes.md"),
	}, envexec.Env{Worktree: worktree}, codex.Options{Bin: bin}, git.New("git"), head
}

func TestVerifierAcceptsAnEditedFinalReport(t *testing.T) {
	filtered := `Hi @octocat.

## Summary

One verified issue remains.

## Blockers

None.

## Critical

- The retry loop has no upper bound.

## Suggestions

None.

## Questions

None.
`
	run, env, opts, g, head := fakeVerifier(t, filtered, false)
	err := (&Runner{Git: g}).verify(context.Background(), Options{ReviewTimeout: 5 * time.Second},
		run, env, opts, "verify", head, NopReporter{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifierMayAddAFindingAndRecordsItLocally(t *testing.T) {
	added := `Hi @octocat.

## Summary

One additional verified issue.

## Blockers

- ` + "`renewLease`" + ` ignores the caller's cancelled context before writing.

## Critical

None.

## Suggestions

None.

## Questions

None.
`
	run, env, opts, g, head := fakeVerifier(t, added, false)
	err := (&Runner{Git: g}).verify(context.Background(), Options{ReviewTimeout: 5 * time.Second},
		run, env, opts, "verify", head, NopReporter{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	changes, err := os.ReadFile(run.changes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changes), "`renewLease` ignores the caller's cancelled context") {
		t.Fatalf("added finding missing from audit log:\n%s", changes)
	}
}

func TestVerifierFailsClosedWhenItChangesTheWorktree(t *testing.T) {
	run, env, opts, g, head := fakeVerifier(t, goodComment, true)
	err := (&Runner{Git: g}).verify(context.Background(), Options{ReviewTimeout: 5 * time.Second},
		run, env, opts, "verify", head, NopReporter{})
	if !errors.Is(err, ErrVerifierInvalid) || !strings.Contains(err.Error(), "untracked changes") {
		t.Fatalf("verify error = %v, want dirty-worktree ErrVerifierInvalid", err)
	}
}

func TestVerifierFailsClosedAfterTwoMalformedReports(t *testing.T) {
	run, env, opts, g, head := fakeVerifier(t, "not a review report\n", false)
	err := (&Runner{Git: g}).verify(context.Background(), Options{ReviewTimeout: 5 * time.Second},
		run, env, opts, "verify", head, NopReporter{})
	if !errors.Is(err, ErrVerifierInvalid) || !strings.Contains(err.Error(), "after 2 attempts") {
		t.Fatalf("verify error = %v, want two-attempt ErrVerifierInvalid", err)
	}
}

func TestVerifierRejectsAnUnexpectedHead(t *testing.T) {
	run, _, _, g, _ := fakeVerifier(t, goodComment, false)
	err := (&Runner{Git: g}).verifierWorktreeUnchanged(context.Background(), run.worktree,
		"ffffffffffffffffffffffffffffffffffffffff")
	if err == nil || !strings.Contains(err.Error(), "changed HEAD") {
		t.Fatalf("HEAD check error = %v", err)
	}
}

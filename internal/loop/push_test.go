package loop

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/target"
)

// A rejected push only reaches a fix session when the rejection is the
// repository's own verification. Every failure git describes in its own words
// - someone else pushed, credentials, a protected branch, the network - must
// end the run instead, because repairing those means touching the remote or
// other people's commits unattended.
func TestFixablePushRejectionOnlyCoversLocalVerification(t *testing.T) {
	unfixable := map[string]string{
		"someone else pushed": "! [rejected] feature -> feature (non-fast-forward)\nhint: Updates were rejected because the tip of your current branch is behind",
		"credentials":         "fatal: Authentication failed for 'https://example.invalid/acme/api.git/'",
		"no write access":     "remote: Permission to acme/api.git denied to example-user",
		"protected branch":    "remote: error: GH006: Protected branch update failed",
		"network":             "ssh: Could not resolve host: example.invalid",
		"nothing to read":     "   \n",
	}
	for name, out := range unfixable {
		if fixablePushRejection(&pushRejection{out: out}) {
			t.Errorf("%s must not be handed to a fix session: %q", name, out)
		}
	}

	hook := "pre-push hook\nsrc/app.ts(12,3): error TS2339: Property 'crumb' does not exist"
	if !fixablePushRejection(&pushRejection{hookOut: hook, local: true}) {
		t.Error("a verification hook rejection must be repairable")
	}
	if fixablePushRejection(&pushRejection{hookOut: hook}) {
		t.Error("unverified output must not be handed to a fix session")
	}
}

func TestTouchesHookConfigCoversTheUsualHookFiles(t *testing.T) {
	for _, path := range []string{
		"lefthook.yml",
		"lefthook-local.yaml",
		"Makefile",
		".golangci.yml",
		".golangci.toml",
		"lefthook.toml",
		".pre-commit-config.yml",
		".pre-commit-config.yaml",
		RepoTestCmdPath,
		".husky/pre-push",
		"tools/.githooks/pre-push",
	} {
		if !touchesHookConfig(path) {
			t.Errorf("%s is hook configuration", path)
		}
	}
	for _, path := range []string{
		"frontend/package.json",
		"tools/Makefile",
		"frontend/src/app.ts",
		"backend/api/crumb_tray.go",
		"docs/reference.md",
	} {
		if touchesHookConfig(path) {
			t.Errorf("%s is not hook configuration", path)
		}
	}
}

func TestPushFixPromptForbidsBypassingTheVerification(t *testing.T) {
	got := pushFixPrompt("feature/crumb-tray", "error TS2339: Property 'crumb' does not exist")
	for _, want := range []string{
		"feature/crumb-tray",
		"error TS2339",
		"Do not push",
		"no --no-verify",
		"no changes to the hook configuration",
		"a line that is exactly:\n" + MarkerComment,
		"never mention AI",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("push fix prompt is missing %q", want)
		}
	}
}

// fakeFixer stands in for the fix session: it records the prompts it was given
// and writes the final message the pipeline reads back.
type fakeFixer struct {
	prompts []string
	onExec  func()
}

func (f *fakeFixer) Exec(_ context.Context, _ envexec.Env, _ time.Duration, prompt, outFile string, _ io.Writer) (engine.SessionRef, error) {
	f.prompts = append(f.prompts, prompt)
	if f.onExec != nil {
		f.onExec()
	}
	return "fake-session", os.WriteFile(outFile, []byte("fixed the type error\n"), 0o644)
}

func (f *fakeFixer) Resume(_ context.Context, _ envexec.Env, _ time.Duration, _ engine.SessionRef, prompt, outFile string, _ io.Writer) error {
	f.prompts = append(f.prompts, prompt)
	if f.onExec != nil {
		f.onExec()
	}
	return os.WriteFile(outFile, []byte("fixed the type error\n"), 0o644)
}

func noCommitPushFixRun(t *testing.T, remoteAfterFailure string) *run {
	t.Helper()
	dir := t.TempDir()
	queried := filepath.Join(dir, "queried")
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
set -eu
case "$1 $2" in
  "rev-parse HEAD") echo "head-sha" ;;
  "rev-parse --path-format=absolute") echo "$PWD/.githooks/pre-push" ;;
  "remote get-url") echo "example.invalid:acme/api.git" ;;
  "hook run") echo "Error: parallel golangci-lint is running"; exit 1 ;;
  "ls-remote origin")
    if [ -f "` + queried + `" ]; then
      printf '` + remoteAfterFailure + `\trefs/heads/feature/crumb-tray\n'
    else
      touch "` + queried + `"
      printf 'base-sha\trefs/heads/feature/crumb-tray\n'
    fi ;;
  "status --porcelain") ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{RepoRoot: dir},
		ctx:      context.Background(),
		rep:      NopReporter{},
		target:   target.Target{BranchOnly: true},
		branch:   "feature/crumb-tray",
		worktree: dir,
		logDir:   dir,
		msgDir:   dir,
		env:      envexec.Env{Worktree: dir},
		fixer:    &fakeFixer{},
	}
}

// pushFixRun builds a run whose fake git rejects the first push with hook
// output and accepts the second one, once the fake fix session has produced a
// commit. changed is what the fix commit touched.
func pushFixRun(t *testing.T, changed string, outOfBandPush bool) (*run, *fakeFixer) {
	t.Helper()
	dir := t.TempDir()
	flag := filepath.Join(dir, "fixed")
	pushed := filepath.Join(dir, "pushed")
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
set -eu
if [ -f "` + flag + `" ]; then head=new-sha; else head=old-sha; fi
case "$1 $2" in
  "rev-parse HEAD") echo "$head" ;;
  "rev-parse --path-format=absolute") echo "$PWD/.githooks/pre-push" ;;
  "push -q")
    if [ "$head" = "new-sha" ]; then touch "` + pushed + `"; exit 0; fi
    echo "pre-push hook"
    echo "src/app.ts(12,3): error TS2339: Property 'crumb' does not exist"
    echo "error: failed to push some refs to 'example.invalid:acme/api.git'"
    exit 1 ;;
  "remote get-url") echo "example.invalid:acme/api.git" ;;
  "hook run")
	input=""
	for arg in "$@"; do case "$arg" in --to-stdin=*) input="${arg#--to-stdin=}" ;; esac; done
	if ! grep -qx "HEAD $head refs/heads/feature/crumb-tray base-sha" "$input"; then
	  echo "unexpected pre-push input" >&2
	  exit 1
	fi
    if [ "$head" = "new-sha" ]; then exit 0; fi
    echo "pre-push hook"
    echo "src/app.ts(12,3): error TS2339: Property 'crumb' does not exist"
    exit 1 ;;
  "ls-remote origin") if [ -f "` + pushed + `" ]; then printf 'new-sha\trefs/heads/feature/crumb-tray\n'; else printf 'base-sha\trefs/heads/feature/crumb-tray\n'; fi ;;
  "status --porcelain") ;;
  "diff --no-renames") echo "` + changed + `" ;;
  "log --oneline") echo "new-sha fix: satisfy the type check" ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fixer := &fakeFixer{onExec: func() {
		if err := os.WriteFile(flag, nil, 0o644); err != nil {
			t.Error(err)
		}
		if outOfBandPush {
			if err := os.WriteFile(pushed, nil, 0o644); err != nil {
				t.Error(err)
			}
		}
	}}
	return &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{RepoRoot: dir},
		ctx:      context.Background(),
		rep:      NopReporter{},
		target:   target.Target{BranchOnly: true},
		branch:   "feature/crumb-tray",
		worktree: dir,
		logDir:   dir,
		msgDir:   dir,
		env:      envexec.Env{Worktree: dir},
		fixer:    fixer,
	}, fixer
}

func TestPushBranchRejectsAFailedPushEvenWhenTheRemoteMatches(t *testing.T) {
	dir := t.TempDir()
	pushed := filepath.Join(dir, "pushed")
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
set -eu
case "$1 $2" in
  "rev-parse HEAD") echo "head-sha" ;;
  "rev-parse --path-format=absolute") echo "$PWD/.githooks/pre-push" ;;
  "push -q") touch "` + pushed + `"; echo "transport failed"; exit 1 ;;
  "remote get-url") echo "example.invalid:acme/api.git" ;;
  "hook run") exit 0 ;;
  "ls-remote origin") if [ -f "` + pushed + `" ]; then printf 'head-sha\trefs/heads/feature/crumb-tray\n'; else printf 'base-sha\trefs/heads/feature/crumb-tray\n'; fi ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p: &Pipeline{Git: git.New(bin)}, o: Options{RepoRoot: dir}, ctx: context.Background(), rep: NopReporter{},
		target: target.Target{BranchOnly: true}, branch: "feature/crumb-tray", worktree: dir, logDir: dir,
	}
	err := r.pushBranch()
	var rejected *pushRejection
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("err = %v, want the original failed push", err)
	}
}

func TestPushBranchWithFixesAcceptsAnUnchangedHeadAlreadyOnTheRemote(t *testing.T) {
	r := noCommitPushFixRun(t, "head-sha")
	if err := r.pushBranchWithFixes(); err != nil {
		t.Fatalf("matching remote after transient hook failure: %v", err)
	}
	if r.headSHA != "head-sha" {
		t.Fatalf("accepted head = %q, want head-sha", r.headSHA)
	}
}

func TestPushBranchWithFixesStillRejectsNoProgressBeforeTheRemoteHasTheHead(t *testing.T) {
	r := noCommitPushFixRun(t, "base-sha")
	err := r.pushBranchWithFixes()
	if !errors.Is(err, ErrNoProgress) {
		t.Fatalf("err = %v, want ErrNoProgress", err)
	}
}

// The whole point of the step: a push the repository's verification refused is
// repaired and pushed, instead of losing every review round of the run.
func TestPushBranchWithFixesRepairsARejectedPush(t *testing.T) {
	r, fixer := pushFixRun(t, "frontend/src/app.ts", false)
	if err := r.pushBranchWithFixes(); err != nil {
		t.Fatalf("push after fix: %v", err)
	}
	if r.headSHA != "new-sha" {
		t.Fatalf("pushed head = %q, want the repaired commit", r.headSHA)
	}
	if r.pushFixTotal != 1 {
		t.Fatalf("push fixes = %d, want 1", r.pushFixTotal)
	}
	if len(fixer.prompts) != 1 || !strings.Contains(fixer.prompts[0], "error TS2339") {
		t.Fatalf("the fix session did not receive the rejection output: %v", fixer.prompts)
	}
}

// Silencing the verification is the shortest way out of a rejected push, so a
// fix that edits the hook configuration stops the run.
func TestPushFixMayNotRewriteTheVerification(t *testing.T) {
	r, _ := pushFixRun(t, "lefthook.yml", false)
	err := r.pushBranchWithFixes()
	if err == nil || !strings.Contains(err.Error(), "lefthook.yml") {
		t.Fatalf("err = %v, want the run stopped over the hook configuration", err)
	}
}

func TestPushFixMayNotPushOutsideThePipeline(t *testing.T) {
	r, _ := pushFixRun(t, "frontend/src/app.ts", true)
	err := r.pushBranchWithFixes()
	if err == nil || !strings.Contains(err.Error(), "out-of-band push") {
		t.Fatalf("err = %v, want the run stopped over the out-of-band push", err)
	}
}

func TestPushFixMayNotRewriteTheGateThroughATestRepair(t *testing.T) {
	dir := t.TempDir()
	pushFixed := filepath.Join(dir, "push-fixed")
	testFixed := filepath.Join(dir, "test-fixed")
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
set -eu
if [ -f "` + pushFixed + `" ]; then head=new-sha; else head=old-sha; fi
case "$1 $2" in
  "rev-parse HEAD") echo "$head" ;;
  "rev-parse --path-format=absolute") echo "$PWD/.githooks/pre-push" ;;
  "push -q")
    if [ "$head" = "new-sha" ]; then exit 0; fi
    echo "pre-push hook"
    exit 1 ;;
  "remote get-url") echo "example.invalid:acme/api.git" ;;
  "hook run")
    if [ "$head" = "new-sha" ]; then exit 0; fi
    echo "pre-push hook"
    exit 1 ;;
  "ls-remote origin") printf 'base-sha\trefs/heads/feature/crumb-tray\n' ;;
  "status --porcelain") ;;
  "diff --no-renames")
    if [ -f "` + testFixed + `" ]; then echo ".golangci.yml"; else echo "src/app.ts"; fi ;;
  "log --oneline") echo "new-sha fix: satisfy the type check" ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	fixer := &fakeFixer{onExec: func() {
		calls++
		path := pushFixed
		if calls == 2 {
			path = testFixed
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Error(err)
		}
	}}
	r := &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{RepoRoot: dir, TestCmd: "test -f " + testFixed, MaxCIFixes: 1},
		ctx:      context.Background(),
		rep:      NopReporter{},
		target:   target.Target{BranchOnly: true},
		branch:   "feature/crumb-tray",
		worktree: dir,
		logDir:   dir,
		msgDir:   dir,
		env:      envexec.Env{Worktree: dir},
		fixer:    fixer,
	}
	err := r.pushBranchWithFixes()
	if err == nil || !strings.Contains(err.Error(), ".golangci.yml") {
		t.Fatalf("err = %v, want the run stopped over the test repair gate change", err)
	}
}

// A branch someone else moved is not a repair job: no session may start, and
// the caller sees the original rejection.
func TestPushBranchWithFixesLeavesANonFastForwardAlone(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
set -eu
case "$1 $2" in
  "rev-parse HEAD") echo "head-sha" ;;
  "push -q")
    echo "! [rejected] feature/crumb-tray -> feature/crumb-tray (non-fast-forward)"
    exit 1 ;;
  "remote get-url") echo "example.invalid:acme/api.git" ;;
  "hook run") exit 0 ;;
  "ls-remote origin") printf 'other-sha\trefs/heads/feature/crumb-tray\n' ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{RepoRoot: dir},
		ctx:      context.Background(),
		rep:      NopReporter{},
		target:   target.Target{BranchOnly: true},
		branch:   "feature/crumb-tray",
		worktree: dir,
		logDir:   dir,
		msgDir:   dir,
		env:      envexec.Env{Worktree: dir},
	}
	err := r.pushBranchWithFixes()
	var rejected *pushRejection
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v, want the push rejection itself", err)
	}
	if r.pushFixTotal != 0 {
		t.Fatalf("push fixes = %d, want none", r.pushFixTotal)
	}
}

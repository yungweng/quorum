package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/review"
)

// The offline loop's whole point is that nothing reaches origin before the
// final push, so a session-side push would defeat it silently. The standing
// rules still carry the exact push command: the CI repair steps after the
// final push are the one place a step tells the session to push, and a bare
// `git push` fails on the detached checkout.
func TestStandingRulesOfflineDeferThePushToThePipeline(t *testing.T) {
	rules := standingRules("feature/crumb-tray", true, false, true, "")
	if !strings.Contains(rules, "Do not push. The pipeline pushes the branch itself") {
		t.Error("offline standing rules do not defer the push to the pipeline")
	}
	if !strings.Contains(rules, "git push origin HEAD:refs/heads/feature/crumb-tray") {
		t.Error("offline standing rules lost the exact push command CI repairs need")
	}
	if !strings.Contains(rules, "one push and one CI run at the very end") {
		t.Error("offline standing rules do not describe the offline pipeline")
	}

	online := standingRules("feature/crumb-tray", true, false, false, "")
	if !strings.Contains(online, "Only push when a step explicitly tells you to") {
		t.Error("online standing rules changed")
	}
}

func TestOfflineRoundPromptsForbidPushing(t *testing.T) {
	for name, prompt := range map[string]string{
		"fix round pr":      fixRoundPrompt(12, "", false, true, "## Summary\n\nfine"),
		"fix round branch":  fixRoundPrompt(0, "feature/crumb-tray", true, true, "## Summary\n\nfine"),
		"suggestion pr":     suggestionRoundPrompt(12, "", false, true, "## Summary\n\nfine"),
		"suggestion branch": suggestionRoundPrompt(0, "feature/crumb-tray", true, true, "## Summary\n\nfine"),
	} {
		if !strings.Contains(prompt, "Do not push") {
			t.Errorf("%s prompt does not forbid pushing", name)
		}
		if strings.Contains(prompt, ", and push") {
			t.Errorf("%s prompt still tells the session to push", name)
		}
		if strings.Contains(prompt, "Do not wait for CI") {
			t.Errorf("%s prompt mentions a CI wait no offline round has", name)
		}
	}
	conflict := conflictFixPrompt(12, "main", "feature/crumb-tray", false, true)
	if !strings.Contains(conflict, "Do not push") || strings.Contains(conflict, ", and push") {
		t.Errorf("offline conflict prompt still tells the session to push: %q", conflict)
	}
}

func TestTestFixPromptCarriesCommandOutputAndCommentContract(t *testing.T) {
	got := testFixPrompt("make check", "FAIL: TestCrumbTray")
	for _, want := range []string{
		"make check",
		"FAIL: TestCrumbTray",
		"Do not push",
		"a line that is exactly:\n" + MarkerComment,
		"never mention AI",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("test fix prompt is missing %q", want)
		}
	}
}

// The gate itself is deterministic: no configured command passes, a green
// command passes, and a red one with no repair budget left is ErrTestsRed
// rather than a silent push of failing code.
func TestEnsureTestsGreenGatesOnTheConfiguredCommand(t *testing.T) {
	dir := t.TempDir()
	r := &run{
		ctx:    context.Background(),
		rep:    NopReporter{},
		logDir: dir,
		env:    envexec.Env{Worktree: dir},
	}

	if err := r.ensureTestsGreen(); err != nil {
		t.Fatalf("no configured command must pass: %v", err)
	}

	r.o.TestCmd = "true"
	if err := r.ensureTestsGreen(); err != nil {
		t.Fatalf("a green command must pass: %v", err)
	}

	r.o.TestCmd = "false"
	r.o.MaxCIFixes = 0
	if err := r.ensureTestsGreen(); !errors.Is(err, ErrTestsRed) {
		t.Fatalf("err = %v, want ErrTestsRed", err)
	}
}

// The repo's own gate must come from the base branch, never from the change
// under review: the fake git only answers for origin/main and fails on any
// other call, so a read from the worktree or the PR head would blow up here.
func TestResolveRepoTestCmdReadsTheBaseBranchVersion(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "rev-parse -q --verify origin/main:.quorum/testcmd") echo "blob-sha" ;;
  "show origin/main:.quorum/testcmd") echo "make check" ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRepoTestCmd(context.Background(), git.New(bin), t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != "make check" {
		t.Fatalf("test command = %q, want %q", got, "make check")
	}
}

// A base branch without the file simply means no repo-provided gate.
func TestResolveRepoTestCmdMissingFileMeansNoGate(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "rev-parse -q --verify origin/main:.quorum/testcmd") exit 1 ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRepoTestCmd(context.Background(), git.New(bin), t.TempDir(), "main")
	if err != nil || got != "" {
		t.Fatalf("resolveRepoTestCmd = %q, %v; want empty and no error", got, err)
	}
}

func TestFinalOfflineReviewCommentSkipsAHeadMovedBySuggestions(t *testing.T) {
	r := &run{
		o:       Options{Post: true},
		rep:     NopReporter{},
		headSHA: "suggestion-head",
	}
	res := &Result{}
	if err := r.postFinalReviewComment(res, 1, review.Findings{
		HeadSHA:     "reviewed-head",
		CommentFile: "review.md",
	}); err != nil {
		t.Fatal(err)
	}
	if res.LastFindings.Posted {
		t.Fatal("stale review comment was marked posted")
	}
}

func TestFinalOfflineReviewCommentRejectsHeadDrift(t *testing.T) {
	dir := t.TempDir()
	commented := filepath.Join(dir, "commented")
	bin := filepath.Join(dir, "gh")
	script := "#!/bin/sh\n" +
		"if test \"$1\" = pr && test \"$2\" = view; then printf foreign-head; exit 0; fi\n" +
		"if test \"$1\" = pr && test \"$2\" = comment; then touch '" + commented + "'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p:       &Pipeline{GH: gh.New(bin)},
		o:       Options{RepoRoot: dir, Post: true},
		ctx:     context.Background(),
		rep:     NopReporter{},
		pr:      gh.FullPR{Number: 42},
		headSHA: "pushed-head",
	}
	err := r.postFinalReviewComment(&Result{}, 1, review.Findings{
		HeadSHA:     "pushed-head",
		CommentFile: "review.md",
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to post") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(commented); !os.IsNotExist(err) {
		t.Fatalf("head-drifted review reached the comment command: %v", err)
	}
}

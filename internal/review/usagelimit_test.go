package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/target"
	"github.com/yungweng/quorum/internal/usagelimit"
)

func TestReviewEngineDefaultsHelper(t *testing.T) {
	eng, model, effort := ReviewEngineDefaults("", "", "")
	if eng != engine.Codex || model != DefaultModel || effort != DefaultEffort {
		t.Fatalf("empty input resolved to %s/%s/%s", eng, model, effort)
	}
	eng, model, effort = ReviewEngineDefaults(engine.Claude, "", "high")
	if eng != engine.Claude || model != ClaudeDefaultModel {
		t.Fatalf("claude resolved to %s/%s", eng, model)
	}
	if effort != "high" {
		t.Fatalf("an explicit effort was rewritten to %q", effort)
	}
	if _, model, _ = ReviewEngineDefaults(engine.Claude, "opus", ""); model != "opus" {
		t.Fatalf("an explicit model was rewritten to %q", model)
	}
	eng, model, effort = ReviewEngineDefaults(engine.Grok, "", "medium")
	if eng != engine.Grok || model != GrokDefaultModel {
		t.Fatalf("grok resolved to %s/%s", eng, model)
	}
	if effort != "medium" {
		t.Fatalf("an explicit grok effort was rewritten to %q", effort)
	}
	if _, model, _ = ReviewEngineDefaults(engine.Grok, "custom-model", ""); model != "custom-model" {
		t.Fatalf("an explicit grok model was rewritten to %q", model)
	}
}

func TestWithDefaultsAppliesCodexModelOnlyForCodexEngine(t *testing.T) {
	o := Options{}.withDefaults()
	if o.Model != DefaultModel || o.Effort != DefaultEffort || o.Engine != engine.Codex {
		t.Fatalf("codex defaults missing: %s/%s/%s", o.Engine, o.Model, o.Effort)
	}
	c := Options{Engine: engine.Claude}.withDefaults()
	if c.Model != ClaudeDefaultModel {
		t.Fatalf("claude default model = %q", c.Model)
	}
	if strings.HasPrefix(c.Model, "gpt-") {
		t.Fatal("a GPT model reached the claude engine")
	}
	g := Options{Engine: engine.Grok}.withDefaults()
	if g.Model != GrokDefaultModel {
		t.Fatalf("grok default model = %q", g.Model)
	}
	if strings.HasPrefix(g.Model, "gpt-") {
		t.Fatal("a GPT model reached the grok engine")
	}
}

// Each engine's effort is checked against its own levels. "ultra" exists for
// codex and not for claude or grok; accepting it for those would run the panel
// at an effort nobody chose.
func TestValidateChecksEffortAgainstTheSelectedEngine(t *testing.T) {
	ok := Options{Engine: engine.Claude, Repo: "acme/api", RepoRoot: "/tmp/x", Effort: "xhigh"}.withDefaults()
	if err := ok.validate(); err != nil {
		t.Fatalf("valid claude effort rejected: %v", err)
	}
	claude := Options{Engine: engine.Claude, Repo: "acme/api", RepoRoot: "/tmp/x", Effort: "ultra"}.withDefaults()
	if err := claude.validate(); err == nil {
		t.Fatal("the codex-only effort ultra was accepted for the claude engine")
	}
	codexOK := Options{Engine: engine.Codex, Repo: "acme/api", RepoRoot: "/tmp/x", Effort: "ultra"}.withDefaults()
	if err := codexOK.validate(); err != nil {
		t.Fatalf("valid codex effort rejected: %v", err)
	}
	grokOK := Options{Engine: engine.Grok, Repo: "acme/api", RepoRoot: "/tmp/x", Effort: "high"}.withDefaults()
	if err := grokOK.validate(); err != nil {
		t.Fatalf("valid grok effort rejected: %v", err)
	}
	grokBad := Options{Engine: engine.Grok, Repo: "acme/api", RepoRoot: "/tmp/x", Effort: "ultra"}.withDefaults()
	if err := grokBad.validate(); err == nil {
		t.Fatal("the codex-only effort ultra was accepted for the grok engine")
	}
	bad := Options{Engine: "gemini", Repo: "acme/api", RepoRoot: "/tmp/x"}
	bad.Runs, bad.Concurrency = 1, 1
	bad.Model, bad.Effort = "m", "medium"
	if err := bad.validate(); err == nil {
		t.Fatal("unknown engine was accepted")
	}
}

// fakeEngineBin writes a codex stand-in whose call count lands in countFile.
func fakeEngineBin(t *testing.T, countFile, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	body := fmt.Sprintf("#!/bin/sh\necho x >> %q\n%s", countFile, script)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func calls(t *testing.T, countFile string) int {
	t.Helper()
	b, err := os.ReadFile(countFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

func limitScript() string {
	return `echo "ERROR: You've hit your usage limit. Visit https://example.com or try again at Aug 10th, 2026 7:32 PM." >&2
exit 1`
}

func testRunPaths(t *testing.T) runPaths {
	t.Helper()
	root := t.TempDir()
	out := filepath.Join(root, "output")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	run := runPaths{
		root: root, output: out,
		worktree:  filepath.Join(root, "worktree"),
		all:       filepath.Join(out, "all-reviewers.md"),
		candidate: filepath.Join(out, "aggregated-pr-comment.md"),
		comment:   filepath.Join(out, "final-pr-comment.md"),
		changes:   filepath.Join(out, "verification-changes.md"),
	}
	if err := os.WriteFile(run.all, []byte("# Reviewer Outputs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return run
}

// The retry exists for malformed output. A usage limit is account-global, so
// a second attempt pays startup cost to hit the same wall; the reviewer
// outputs stay on disk for a resume instead.
func TestAggregateStopsAfterOneAttemptOnUsageLimit(t *testing.T) {
	run := testRunPaths(t)
	count := filepath.Join(t.TempDir(), "count")
	eng := codex.Options{Bin: fakeEngineBin(t, count, limitScript())}

	r := &Runner{}
	err := r.aggregate(context.Background(), Options{ReviewTimeout: time.Minute}, run,
		envexec.Env{Worktree: t.TempDir()}, eng, "prompt", 3, NopReporter{})
	if !errors.Is(err, usagelimit.Err) {
		t.Fatalf("err = %v, want the usage-limit error", err)
	}
	if got := calls(t, count); got != 1 {
		t.Fatalf("aggregator ran %d times, want 1", got)
	}
}

func TestAggregateStillRetriesOrdinaryFailures(t *testing.T) {
	run := testRunPaths(t)
	count := filepath.Join(t.TempDir(), "count")
	eng := codex.Options{Bin: fakeEngineBin(t, count, `echo "boom" >&2
exit 1`)}

	r := &Runner{}
	err := r.aggregate(context.Background(), Options{ReviewTimeout: time.Minute}, run,
		envexec.Env{Worktree: t.TempDir()}, eng, "prompt", 3, NopReporter{})
	if !errors.Is(err, ErrAggregatorInvalid) {
		t.Fatalf("err = %v, want ErrAggregatorInvalid", err)
	}
	if got := calls(t, count); got != 2 {
		t.Fatalf("aggregator ran %d times, want 2", got)
	}
}

// One reviewer hitting the account-global limit dooms the rest; the fan-out
// must come back with the typed error instead of burning the other passes.
func TestRunReviewersStopsAtAUsageLimit(t *testing.T) {
	run := testRunPaths(t)
	count := filepath.Join(t.TempDir(), "count")
	o := Options{Runs: 3, Concurrency: 1, ReviewTimeout: time.Minute}
	eng := codex.Options{Bin: fakeEngineBin(t, count, limitScript())}

	r := &Runner{}
	err := r.runReviewers(context.Background(), o, run, envexec.Env{Worktree: t.TempDir()}, eng, "origin/main", NopReporter{})
	if !errors.Is(err, usagelimit.Err) {
		t.Fatalf("err = %v, want the usage-limit error", err)
	}
}

// Unrelated per-reviewer failures keep their existing meaning: the pass is
// counted as failed and the run continues to the too-few-outputs gate.
func TestRunReviewersToleratesOrdinaryReviewerFailures(t *testing.T) {
	run := testRunPaths(t)
	count := filepath.Join(t.TempDir(), "count")
	o := Options{Runs: 2, Concurrency: 2, ReviewTimeout: time.Minute}
	eng := codex.Options{Bin: fakeEngineBin(t, count, `echo "boom" >&2
exit 1`)}

	r := &Runner{}
	if err := r.runReviewers(context.Background(), o, run, envexec.Env{Worktree: t.TempDir()}, eng, "origin/main", NopReporter{}); err != nil {
		t.Fatalf("ordinary failures must not fail the fan-out: %v", err)
	}
	if got := calls(t, count); got != 2 {
		t.Fatalf("reviewers ran %d times, want 2", got)
	}
}

// A resume must never shrink the panel a run is measured against: two
// surviving outputs of a six-reviewer fan-out are a shortfall, not a complete
// two-reviewer panel. Extra outputs from a larger earlier run stay usable.
func TestRequestedReviewersKeepsTheConfiguredPanelAsTheFloor(t *testing.T) {
	if got := requestedReviewers(6, 2, true); got != 6 {
		t.Fatalf("requestedReviewers(6, 2, resumed) = %d, want 6", got)
	}
	if got := requestedReviewers(2, 6, true); got != 6 {
		t.Fatalf("requestedReviewers(2, 6, resumed) = %d, want 6", got)
	}
	if got := requestedReviewers(6, 4, false); got != 6 {
		t.Fatalf("requestedReviewers(6, 4, fresh) = %d, want 6", got)
	}
}

func TestResumeUnusableWrapsMissingRunDir(t *testing.T) {
	o := Options{ResumeRun: filepath.Join(t.TempDir(), "gone")}
	run, err := o.runDir(0, "")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	_, _, cerr := r.checkout(context.Background(), o, run, target.Target{}, "main", "origin/main")
	if !errors.Is(cerr, ErrResumeUnusable) {
		t.Fatalf("err = %v, want ErrResumeUnusable", cerr)
	}
}

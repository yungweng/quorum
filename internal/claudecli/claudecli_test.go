package claudecli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/usagelimit"
)

func TestFlagsCarryModelNotEffort(t *testing.T) {
	got := strings.Join(Options{Model: "sonnet", Effort: "high"}.flags(), " ")
	if !strings.Contains(got, "--model sonnet") {
		t.Errorf("model flag missing: %s", got)
	}
	if strings.Contains(got, "high") || strings.Contains(got, "effort") {
		t.Errorf("effort leaked into the claude flags: %s", got)
	}
	if !strings.Contains(got, "--output-format json") {
		t.Errorf("json output flag missing: %s", got)
	}
}

// The permission bypass is what lets a fix session run tests, use gh and
// push unattended. The review side must never carry it: its posture is the
// fixed read-only tool set instead.
func TestBypassAddsTheSkipPermissionsFlag(t *testing.T) {
	const bypass = "--dangerously-skip-permissions"
	if !strings.Contains(strings.Join(Options{Bypass: true}.flags(), " "), bypass) {
		t.Error("the bypass flag was not set although it was requested")
	}
	if strings.Contains(strings.Join(Options{}.flags(), " "), bypass) {
		t.Error("the bypass flag appeared without being requested")
	}
	if strings.Contains(strings.Join(readOnlyFlags(), " "), bypass) {
		t.Error("the read-only posture carried the permission bypass")
	}
}

func TestReadOnlyComposesReviewFlags(t *testing.T) {
	got := strings.Join(readOnlyFlags(), " ")
	for _, want := range []string{
		"--tools Read,Grep,Glob",
		"--strict-mcp-config",
		"--permission-mode dontAsk",
		"--no-session-persistence",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("read-only flags are missing %q: %s", want, got)
		}
	}
}

func TestClassifyRecognisesQuotaRefusals(t *testing.T) {
	if classify("Claude usage limit reached, resets at 3am") == nil {
		t.Error("usage limit line was not classified")
	}
	// A per-minute throttle is transient, not account exhaustion: with every
	// reviewer starting at once it happens routinely and must not pause the
	// agent.
	if classify("API rate limit exceeded") != nil {
		t.Error("a transient rate-limit throttle was classified as a quota refusal")
	}
	if classify("something else entirely broke") != nil {
		t.Error("an unrelated failure was classified as a quota refusal")
	}
}

// fakeClaude writes an executable script standing in for the claude binary.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(t *testing.T) envexec.Env {
	t.Helper()
	return envexec.Env{Worktree: t.TempDir()}
}

const envelope = `{"result":"all done","session_id":"sess-1234","is_error":false}`

func TestExecReturnsSessionIDFromJSONAndWritesTheResult(t *testing.T) {
	bin := fakeClaude(t, `echo '`+envelope+`'`)
	out := filepath.Join(t.TempDir(), "out.md")
	id, err := Options{Bin: bin}.Exec(context.Background(), testEnv(t), 0, "prompt", out, io.Discard)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if id != "sess-1234" {
		t.Errorf("session id = %s", id)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "all done" {
		t.Errorf("outFile = %q, want the envelope's result text", b)
	}
}

func TestExecRejectsAnEnvelopeWithoutASessionID(t *testing.T) {
	bin := fakeClaude(t, `echo '{"result":"x","is_error":false}'`)
	if _, err := (Options{Bin: bin}).Exec(context.Background(), testEnv(t), 0, "p", filepath.Join(t.TempDir(), "o.md"), io.Discard); err == nil {
		t.Fatal("want an error when no session id comes back")
	}
}

func TestRunRejectsNonJSONOutput(t *testing.T) {
	bin := fakeClaude(t, `echo "I am not JSON"`)
	err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sid", "p", filepath.Join(t.TempDir(), "o.md"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "JSON envelope") {
		t.Fatalf("err = %v, want the JSON envelope complaint", err)
	}
}

func TestRunClassifiesUsageLimitExit(t *testing.T) {
	bin := fakeClaude(t, `echo "Claude usage limit reached" >&2
exit 1`)
	err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sid", "p", filepath.Join(t.TempDir(), "o.md"), io.Discard)
	if !errors.Is(err, usagelimit.Err) {
		t.Fatalf("err = %v, want the usage-limit error", err)
	}
}

// The envelope's result text quotes whatever the model just read, including
// code or docs that mention usage limits. A nonzero exit after printing it
// must stay an ordinary failure: only stderr feeds the refusal tail.
func TestRunIgnoresUsageLimitQuotedOnStdout(t *testing.T) {
	bin := fakeClaude(t, `echo '{"result":"the diff handles your usage limit marker","session_id":"s","is_error":false}'
echo "unrelated crash" >&2
exit 1`)
	err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sid", "p", filepath.Join(t.TempDir(), "o.md"), io.Discard)
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, usagelimit.Err) {
		t.Fatalf("quoted stdout text classified as a refusal: %v", err)
	}
}

func TestRunClassifiesUsageLimitInErrorEnvelope(t *testing.T) {
	bin := fakeClaude(t, `echo '{"result":"You have hit your usage limit for today","session_id":"s","is_error":true}'`)
	err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sid", "p", filepath.Join(t.TempDir(), "o.md"), io.Discard)
	if !errors.Is(err, usagelimit.Err) {
		t.Fatalf("err = %v, want the usage-limit error", err)
	}
}

func TestResumePassesTheSessionID(t *testing.T) {
	bin := fakeClaude(t, `echo "$@" >&2
echo '`+envelope+`'`)
	log := filepath.Join(t.TempDir(), "run.log")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sess-9999", "p", filepath.Join(t.TempDir(), "o.md"), f); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--resume sess-9999") {
		t.Errorf("resume args missing from invocation: %s", b)
	}
}

// The reviewer pass computes the diff itself and feeds it with the prompt on
// stdin; the fake asserts both arrive and that the read-only posture is on.
func TestReviewFeedsDiffAndPromptOnStdin(t *testing.T) {
	env := testEnv(t)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = env.Worktree
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(env.Worktree, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "base")
	git("branch", "base")
	if err := os.WriteFile(filepath.Join(env.Worktree, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-aqm", "change")

	bin := fakeClaude(t, `stdin=$(cat)
case "$stdin" in
*"reviewer on an independent code review panel"*) ;;
*) echo "prompt missing" >&2; exit 1 ;;
esac
case "$stdin" in
*"+two"*) ;;
*) echo "diff missing" >&2; exit 1 ;;
esac
case "$@" in
*"--tools Read,Grep,Glob"*) ;;
*) echo "read-only posture missing" >&2; exit 1 ;;
esac
echo '`+envelope+`'`)
	out := filepath.Join(t.TempDir(), "reviewer-1.md")
	if err := (Options{Bin: bin}).Review(context.Background(), env, 0, "base", out, io.Discard); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "all done" {
		t.Errorf("reviewer output = %q", b)
	}
}

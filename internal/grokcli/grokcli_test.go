package grokcli

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

func TestFlagsCarryModelAndEffort(t *testing.T) {
	got := strings.Join(Options{Model: "grok-4.5", Effort: "high"}.flags(), " ")
	if !strings.Contains(got, "--model grok-4.5") {
		t.Errorf("model flag missing: %s", got)
	}
	if !strings.Contains(got, "--effort high") {
		t.Errorf("effort flag missing: %s", got)
	}
	if !strings.Contains(got, "--output-format json") {
		t.Errorf("json output flag missing: %s", got)
	}
	if strings.Contains(strings.Join(Options{Model: "grok-4.5"}.flags(), " "), "--effort") {
		t.Error("an unset effort still produced an --effort flag")
	}
}

// Grok rejects an effort level it does not know, but the accepted set is
// model-menu specific. A codex-only level must not pass validation here.
func TestValidEffortRejectsUnknownLevels(t *testing.T) {
	for _, level := range []string{"minimal", "ultra", "xhigh", "max", "insane", "HIGH"} {
		if ValidEffort(level) {
			t.Errorf("%q was accepted as a grok effort", level)
		}
	}
	for _, level := range []string{"", "low", "medium", "high"} {
		if !ValidEffort(level) {
			t.Errorf("%q was rejected as a grok effort", level)
		}
	}
}

// The permission bypass is what lets a fix session run tests, use gh and
// push unattended. The review side must never carry it: its posture is the
// fixed read-only tool set instead.
func TestBypassAddsTheAlwaysApproveFlag(t *testing.T) {
	const bypass = "--always-approve"
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
		"--tools read_file,grep,list_dir",
		"--permission-mode dontAsk",
		"--sandbox read-only",
		"--disable-web-search",
		"--disallowed-tools Agent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("read-only flags are missing %q: %s", want, got)
		}
	}
}

func TestClassifyRecognisesQuotaRefusals(t *testing.T) {
	if classify("Grok usage limit reached, try again later") == nil {
		t.Error("usage limit line was not classified")
	}
	if classify("API rate limit exceeded") != nil {
		t.Error("a transient rate-limit throttle was classified as a quota refusal")
	}
	if classify("something else entirely broke") != nil {
		t.Error("an unrelated failure was classified as a quota refusal")
	}
}

// fakeGrok writes an executable script standing in for the grok binary.
func fakeGrok(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(t *testing.T) envexec.Env {
	t.Helper()
	return envexec.Env{Worktree: t.TempDir()}
}

const envelope = `{"text":"all done","sessionId":"sess-1234"}`

func TestExecReturnsSessionIDFromJSONAndWritesTheResult(t *testing.T) {
	bin := fakeGrok(t, `echo '`+envelope+`'`)
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
	bin := fakeGrok(t, `echo '{"text":"x"}'`)
	if _, err := (Options{Bin: bin}).Exec(context.Background(), testEnv(t), 0, "p", filepath.Join(t.TempDir(), "o.md"), io.Discard); err == nil {
		t.Fatal("want an error when no session id comes back")
	}
}

func TestRunRejectsNonJSONOutput(t *testing.T) {
	bin := fakeGrok(t, `echo "I am not JSON"`)
	err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sid", "p", filepath.Join(t.TempDir(), "o.md"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "JSON envelope") {
		t.Fatalf("err = %v, want the JSON envelope complaint", err)
	}
}

func TestRunClassifiesUsageLimitExit(t *testing.T) {
	bin := fakeGrok(t, `echo "Grok usage limit reached" >&2
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
	bin := fakeGrok(t, `echo '{"text":"the diff handles your usage limit marker","sessionId":"s"}'
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
	bin := fakeGrok(t, `echo '{"type":"error","message":"You have hit your usage limit for today"}'`)
	err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sid", "p", filepath.Join(t.TempDir(), "o.md"), io.Discard)
	if !errors.Is(err, usagelimit.Err) {
		t.Fatalf("err = %v, want the usage-limit error", err)
	}
}

func TestResumePassesTheSessionID(t *testing.T) {
	bin := fakeGrok(t, `echo "$@" >&2
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

func TestRunUsesPromptFile(t *testing.T) {
	bin := fakeGrok(t, `echo "$@" >&2
# last arg after --prompt-file is the path
found=
for a in "$@"; do
  if [ "$found" = 1 ]; then
    cat "$a" >&2
    break
  fi
  [ "$a" = "--prompt-file" ] && found=1
done
echo '`+envelope+`'`)
	log := filepath.Join(t.TempDir(), "run.log")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "s", "hello-prompt-body", filepath.Join(t.TempDir(), "o.md"), f); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--prompt-file") {
		t.Errorf("prompt-file flag missing: %s", b)
	}
	if !strings.Contains(string(b), "hello-prompt-body") {
		t.Errorf("prompt body missing from file contents: %s", b)
	}
}

// The reviewer pass computes the diff itself and feeds it with the prompt
// via --prompt-file; the fake asserts both arrive and that the read-only
// posture is on.
func TestReviewFeedsDiffAndPromptViaPromptFile(t *testing.T) {
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

	bin := fakeGrok(t, `found=
for a in "$@"; do
  if [ "$found" = 1 ]; then
    stdin=$(cat "$a")
    break
  fi
  [ "$a" = "--prompt-file" ] && found=1
done
case "$stdin" in
*"reviewer on an independent code review panel"*) ;;
*) echo "prompt missing" >&2; exit 1 ;;
esac
case "$stdin" in
*"+two"*) ;;
*) echo "diff missing" >&2; exit 1 ;;
esac
case "$@" in
*"--tools read_file,grep,list_dir"*) ;;
*) echo "read-only posture missing" >&2; exit 1 ;;
esac
case "$@" in
*"--sandbox read-only"*) ;;
*) echo "sandbox missing" >&2; exit 1 ;;
esac
echo '`+envelope+`'`)
	out := filepath.Join(t.TempDir(), "reviewer-1.md")
	if err := (Options{Bin: bin}).Review(context.Background(), env, 0, "base", "", out, io.Discard); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "all done" {
		t.Errorf("reviewer output = %q", b)
	}
}

// Repository rules ride into the reviewer prompt as additional criteria; a
// repo without rules gets exactly the prompt it always got.
func TestReviewPromptCarriesRepositoryRules(t *testing.T) {
	rules := "- No new UI components; reuse existing ones first (Blocker)."
	with := reviewPrompt("origin/main", "diff", rules)
	if !strings.Contains(with, rules) {
		t.Errorf("rules missing from prompt: %s", with)
	}
	if !strings.Contains(with, "real finding you must report") {
		t.Error("rules block lost its severity framing")
	}
	without := reviewPrompt("origin/main", "diff", "")
	if strings.Contains(without, "its own review rules") {
		t.Errorf("rules block appeared without rules: %s", without)
	}
}

package codex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
)

// argv runs call against a fake codex that records its arguments, one per
// line, and returns them.
func argv(t *testing.T, call func(o Options, env envexec.Env, out string) error, o Options) []string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	o.Bin = fakeCodex(t, `for a in "$@"; do printf '%s\n' "$a"; done > "`+record+`"`)
	if err := call(o, testEnv(t), filepath.Join(dir, "out.md")); err != nil {
		t.Fatalf("codex call: %v", err)
	}
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func review(baseRef, rules string) func(Options, envexec.Env, string) error {
	return func(o Options, env envexec.Env, out string) error {
		return o.Review(context.Background(), env, 0, baseRef, rules, out, io.Discard)
	}
}

func resume(o Options, env envexec.Env, out string) error {
	return o.Resume(context.Background(), env, 0, "sid", "prompt", out, io.Discard)
}

func prompt(o Options, env envexec.Env, out string) error {
	return o.Prompt(context.Background(), env, 0, "instruction", out, nil, io.Discard)
}

// Codex has a review-specific model setting that takes precedence over -m. Both
// have to be set, or a user's own config could silently select a different
// model for the reviewers than the one that was asked for and paid for.
func TestReviewSetsBothModelSettings(t *testing.T) {
	got := strings.Join(argv(t, review("origin/main", ""), Options{Model: "gpt-test", Effort: "medium"}), " ")
	if !strings.Contains(got, "-m gpt-test") {
		t.Errorf("the plain model flag is missing: %s", got)
	}
	if !strings.Contains(got, `review_model="gpt-test"`) {
		t.Errorf("the review model override is missing: %s", got)
	}
	if !strings.Contains(got, "model_reasoning_effort=medium") {
		t.Errorf("the effort is missing: %s", got)
	}
}

// The reviewers get no value from Serena, and its per-reviewer server spawns
// have deadlocked runs before.
func TestSerenaIsDisabledWhenAsked(t *testing.T) {
	with := strings.Join(argv(t, review("origin/main", ""), Options{DisableSerena: true}), " ")
	if !strings.Contains(with, "mcp_servers.serena.enabled=false") {
		t.Errorf("serena was not disabled: %s", with)
	}
	without := strings.Join(argv(t, review("origin/main", ""), Options{}), " ")
	if strings.Contains(without, "serena") {
		t.Errorf("serena was disabled without being asked: %s", without)
	}
}

// The sandbox bypass is what lets a fix session run tests, use gh and push
// unattended. It must never appear on the review side; the verifier stays in
// the read-only sandbox.
func TestBypassOnlyWhenRequested(t *testing.T) {
	const bypass = "--dangerously-bypass-approvals-and-sandbox"
	if !strings.Contains(strings.Join(argv(t, resume, Options{Bypass: true}), " "), bypass) {
		t.Error("the bypass flag was not set although it was requested")
	}
	if strings.Contains(strings.Join(argv(t, resume, Options{}), " "), bypass) {
		t.Error("the bypass flag appeared without being requested")
	}
	if strings.Contains(strings.Join(argv(t, review("origin/main", ""), Options{Model: "m"}), " "), bypass) {
		t.Error("a review run carried the sandbox bypass")
	}
	got := strings.Join(argv(t, prompt, Options{}), " ")
	if strings.Contains(got, bypass) || !strings.Contains(got, "--sandbox read-only") {
		t.Errorf("a read-only pass left the read-only sandbox: %s", got)
	}
}

// An empty model or effort has to mean "leave the user's codex defaults alone",
// rather than passing an empty value to -m.
func TestEmptySettingsProduceNoFlags(t *testing.T) {
	got := argv(t, resume, Options{})
	if len(got) != 6 || strings.Join(got[:3], " ") != "exec resume sid" || got[3] != "-o" || got[5] != "prompt" {
		t.Errorf("empty options produced flags: %q", got)
	}
}

func TestTOMLStringEscaping(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:     `"plain"`,
		`say "hi"`:  `"say \"hi\""`,
		`back\side`: `"back\\side"`,
		"tab\there": `"tab\there"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
}

// writeRollout fakes a Codex rollout file in the layout the CLI uses.
func writeRollout(t *testing.T, dir, sessionID, cwd string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-27T00-28-32-"+sessionID+".jsonl")
	line := `{"timestamp":"2026-07-26T22:28:32.200Z","type":"session_meta","payload":` +
		`{"session_id":"` + sessionID + `","cwd":"` + cwd + `","originator":"codex_exec"}}` + "\n" +
		`{"type":"message","payload":{"role":"user"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// Session recovery is what lets every fix round share one session. The safety
// argument is that the worktree path is unique per run, so a session started
// elsewhere on the machine can never be picked up by mistake.
func TestFindSessionMatchesOnTheWorktreePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	now := time.Now()
	today := filepath.Join(home, ".codex", "sessions", now.Format("2006/01/02"))
	ours := "/cache/quorum/babysit/acme-api-pr-12-20260727-002410/worktree"

	writeRollout(t, today, "aaaaaaaa-1111-2222-3333-444444444444", "/somewhere/else", now.Add(-time.Minute))
	writeRollout(t, today, "bbbbbbbb-1111-2222-3333-444444444444", ours, now)

	got, err := findSession(ours)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	if got != "bbbbbbbb-1111-2222-3333-444444444444" {
		t.Errorf("session = %s, want the one whose cwd is our worktree", got)
	}

	if _, err := findSession("/a/worktree/that/never/ran"); err == nil {
		t.Error("a worktree with no session was matched to one anyway")
	}
}

// A run that starts before midnight resumes after it, so yesterday's bucket
// has to be searched too.
func TestFindSessionLooksAtYesterday(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	yesterday := time.Now().AddDate(0, 0, -1)
	dir := filepath.Join(home, ".codex", "sessions", yesterday.Format("2006/01/02"))
	worktree := "/cache/quorum/babysit/acme-api-pr-12/worktree"
	writeRollout(t, dir, "cccccccc-1111-2222-3333-444444444444", worktree, yesterday)

	got, err := findSession(worktree)
	if err != nil {
		t.Fatalf("findSession: %v", err)
	}
	if got != "cccccccc-1111-2222-3333-444444444444" {
		t.Errorf("session = %s", got)
	}
}

// Two sessions in the same worktree means a run that was restarted; the newest
// is the live one.
func TestFindSessionPrefersTheNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	now := time.Now()
	dir := filepath.Join(home, ".codex", "sessions", now.Format("2006/01/02"))
	worktree := "/cache/quorum/babysit/acme-api-pr-12/worktree"

	writeRollout(t, dir, "dddddddd-1111-2222-3333-444444444444", worktree, now.Add(-time.Hour))
	writeRollout(t, dir, "eeeeeeee-1111-2222-3333-444444444444", worktree, now)

	got, err := findSession(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got != "eeeeeeee-1111-2222-3333-444444444444" {
		t.Errorf("session = %s, want the newest", got)
	}
}

// A truncated or unrelated file must be skipped, not crash the lookup.
func TestFindSessionSkipsUnreadableRollouts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	now := time.Now()
	dir := filepath.Join(home, ".codex", "sessions", now.Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-broken.jsonl"), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := "/cache/quorum/babysit/acme-api-pr-12/worktree"
	writeRollout(t, dir, "ffffffff-1111-2222-3333-444444444444", worktree, now)

	got, err := findSession(worktree)
	if err != nil {
		t.Fatalf("a broken rollout file broke the lookup: %v", err)
	}
	if got != "ffffffff-1111-2222-3333-444444444444" {
		t.Errorf("session = %s", got)
	}
}

// Without rules a reviewer pass is the plain --base review it always was. With
// rules it cannot be: codex rejects --base next to the positional
// custom-instructions argument, so the rules ride in a custom prompt that
// carries the base-diff task itself.
func TestReviewSwapsBaseForCustomInstructionsWhenRulesExist(t *testing.T) {
	plain := argv(t, review("origin/main", ""), Options{})
	if got := strings.Join(plain, " "); !strings.Contains(got, "--base origin/main") {
		t.Errorf("--base missing without rules: %s", got)
	}

	rules := "- UI changes must be mobile responsive (Critical)."
	withRules := argv(t, review("origin/main", rules), Options{})
	if slices.Contains(withRules, "--base") {
		t.Errorf("--base conflicts with custom instructions and must not appear: %q", withRules)
	}
	// The prompt spans several lines, so it is everything after the last flag.
	prompt := strings.Join(withRules[slices.Index(withRules, "-o")+2:], "\n")
	for _, want := range []string{"origin/main", "git merge-base HEAD origin/main", "full general review", rules} {
		if !strings.Contains(prompt, want) {
			t.Errorf("custom review prompt is missing %q", want)
		}
	}
}

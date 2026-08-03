package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Codex silently ignores an unknown effort instead of failing, so a typo would
// run every reviewer at the wrong setting and only show up in the bill.
func TestEffortValidation(t *testing.T) {
	for _, ok := range []string{"", "minimal", "low", "medium", "high", "xhigh"} {
		if !ValidEffort(ok) {
			t.Errorf("%q was rejected", ok)
		}
	}
	for _, bad := range []string{"maximum", "Medium", "hi", "0"} {
		if ValidEffort(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// Codex has a review-specific model setting that takes precedence over -m. Both
// have to be set, or a user's own config could silently select a different
// model for the reviewers than the one that was asked for and paid for.
func TestReviewFlagsOverrideBothModelSettings(t *testing.T) {
	got := strings.Join(Options{Model: "gpt-5.6-terra", Effort: "medium"}.reviewFlags(), " ")
	if !strings.Contains(got, "-m gpt-5.6-terra") {
		t.Errorf("the plain model flag is missing: %s", got)
	}
	if !strings.Contains(got, `review_model="gpt-5.6-terra"`) {
		t.Errorf("the review model override is missing: %s", got)
	}
}

// The reviewers get no value from Serena, and its per-reviewer server spawns
// have deadlocked runs before.
func TestSerenaIsDisabledWhenAsked(t *testing.T) {
	with := strings.Join(Options{DisableSerena: true}.flags(), " ")
	if !strings.Contains(with, "mcp_servers.serena.enabled=false") {
		t.Errorf("serena was not disabled: %s", with)
	}
	without := strings.Join(Options{}.flags(), " ")
	if strings.Contains(without, "serena") {
		t.Errorf("serena was disabled without being asked: %s", without)
	}
}

// The sandbox bypass is what lets a fix session run tests, use gh and push
// unattended. It must never appear on the review side; the verifier stays in
// the read-only sandbox.
func TestBypassOnlyWhenRequested(t *testing.T) {
	const bypass = "--dangerously-bypass-approvals-and-sandbox"
	if !strings.Contains(strings.Join(Options{Bypass: true}.flags(), " "), bypass) {
		t.Error("the bypass flag was not set although it was requested")
	}
	if strings.Contains(strings.Join(Options{}.flags(), " "), bypass) {
		t.Error("the bypass flag appeared without being requested")
	}
	if strings.Contains(strings.Join(Options{Model: "m"}.reviewFlags(), " "), bypass) {
		t.Error("a review run carried the sandbox bypass")
	}
}

// An empty model or effort has to mean "leave the user's codex defaults alone",
// rather than passing an empty value to -m.
func TestEmptySettingsProduceNoFlags(t *testing.T) {
	got := Options{}.flags()
	if len(got) != 0 {
		t.Errorf("empty options produced flags: %v", got)
	}
}

func TestTOMLStringEscaping(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:     `"plain"`,
		`say "hi"`:  `"say \"hi\""`,
		`back\side`: `"back\\side"`,
		"tab\there": `"tab\there"`,
	} {
		if got := TOMLString(in); got != want {
			t.Errorf("TOMLString(%q) = %s, want %s", in, got, want)
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

	got, err := FindSession(ours)
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	if got != "bbbbbbbb-1111-2222-3333-444444444444" {
		t.Errorf("session = %s, want the one whose cwd is our worktree", got)
	}

	if _, err := FindSession("/a/worktree/that/never/ran"); err == nil {
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

	got, err := FindSession(worktree)
	if err != nil {
		t.Fatalf("FindSession: %v", err)
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

	got, err := FindSession(worktree)
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

	got, err := FindSession(worktree)
	if err != nil {
		t.Fatalf("a broken rollout file broke the lookup: %v", err)
	}
	if got != "ffffffff-1111-2222-3333-444444444444" {
		t.Errorf("session = %s", got)
	}
}

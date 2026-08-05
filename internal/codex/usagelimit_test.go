package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/usagelimit"
)

// fakeCodex writes an executable script standing in for the codex binary.
func fakeCodex(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(t *testing.T) envexec.Env {
	t.Helper()
	return envexec.Env{Worktree: t.TempDir()}
}

// The refusal names the reset time; classifying it here is what lets the
// agent pause instead of hammering an exhausted account every poll cycle.
func TestExecClassifiesUsageLimitExit(t *testing.T) {
	bin := fakeCodex(t, `echo "ERROR: You've hit your usage limit. Visit https://example.com/usage to purchase more credits or try again at Aug 10th, 2026 7:32 PM." >&2
exit 1`)
	_, err := Options{Bin: bin}.Exec(context.Background(), testEnv(t), 0, "prompt", filepath.Join(t.TempDir(), "out.md"), io.Discard)
	var ul *usagelimit.Error
	if !errors.As(err, &ul) {
		t.Fatalf("err = %v, want *usagelimit.Error", err)
	}
	want := time.Date(2026, time.August, 10, 19, 32, 0, 0, time.Local)
	if !ul.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", ul.ResetAt, want)
	}
}

func TestExecOrdinaryFailureIsNotClassifiedAsUsageLimit(t *testing.T) {
	bin := fakeCodex(t, `echo "ERROR: something else broke" >&2
exit 1`)
	_, err := Options{Bin: bin}.Exec(context.Background(), testEnv(t), 0, "prompt", filepath.Join(t.TempDir(), "out.md"), io.Discard)
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, usagelimit.Err) {
		t.Fatalf("ordinary failure classified as usage limit: %v", err)
	}
}

// A kill after the timeout also ends in a nonzero exit, but it is quorum's
// doing, not a message from codex, and must never read as a usage limit.
func TestExecTimeoutIsNotClassifiedAsUsageLimit(t *testing.T) {
	bin := fakeCodex(t, `echo "hit your usage limit" >&2
sleep 10`)
	_, err := Options{Bin: bin}.Exec(context.Background(), testEnv(t), 50*time.Millisecond, "prompt", filepath.Join(t.TempDir(), "out.md"), io.Discard)
	if !errors.Is(err, proc.ErrTimeout) {
		t.Fatalf("err = %v, want the timeout error", err)
	}
	if errors.Is(err, usagelimit.Err) {
		t.Fatal("a timeout was classified as a usage limit")
	}
}

// Exec now returns the session id itself, so callers no longer race the
// rollout scan; the lookup stays keyed on the unique worktree path.
func TestExecReturnsTheDiscoveredSessionID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	env := testEnv(t)
	today := filepath.Join(home, ".codex", "sessions", time.Now().Format("2006/01/02"))
	writeRollout(t, today, "cccccccc-1111-2222-3333-444444444444", env.Worktree, time.Now())

	bin := fakeCodex(t, "exit 0")
	id, err := Options{Bin: bin}.Exec(context.Background(), env, 0, "prompt", filepath.Join(t.TempDir(), "out.md"), io.Discard)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if id != "cccccccc-1111-2222-3333-444444444444" {
		t.Errorf("session id = %s", id)
	}
}

func TestExecWrapsMissingSessionError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := fakeCodex(t, "exit 0")
	_, err := Options{Bin: bin}.Exec(context.Background(), testEnv(t), 0, "prompt", filepath.Join(t.TempDir(), "out.md"), io.Discard)
	if err == nil {
		t.Fatal("want an error when no rollout matches the worktree")
	}
}

// The usage-limit line lands in the log file as before; the tail is a copy,
// not a diversion.
func TestRunStillWritesTheLog(t *testing.T) {
	bin := fakeCodex(t, `echo "hello from codex"
exit 0`)
	log := filepath.Join(t.TempDir(), "run.log")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := (Options{Bin: bin}).Resume(context.Background(), testEnv(t), 0, "sid", "prompt", filepath.Join(t.TempDir(), "out.md"), f); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" {
		t.Fatal("nothing reached the log file")
	}
}

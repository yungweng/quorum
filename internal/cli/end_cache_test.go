package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/state"
)

func TestEndStateCacheRoundTripsOnlyKnownStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "pr-states.json")
	want := map[string]string{
		"acme/api#1": gh.StateMerged,
		"acme/api#2": gh.StateOpen,
		"acme/api#4": gh.StateUnavailable,
	}
	withInvalid := map[string]string{
		"acme/api#1": gh.StateMerged,
		"acme/api#2": gh.StateOpen,
		"acme/api#3": "NOT_A_GITHUB_STATE",
		"acme/api#4": gh.StateUnavailable,
	}
	if err := writeEndStateCache(path, withInvalid); err != nil {
		t.Fatal(err)
	}
	got := readEndStateCache(path)
	if len(got) != len(want) || got["acme/api#1"] != want["acme/api#1"] || got["acme/api#2"] != want["acme/api#2"] {
		t.Fatalf("cache = %v, want %v", got, want)
	}

	if err := os.WriteFile(path, []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readEndStateCache(path); len(got) != 0 {
		t.Fatalf("corrupt cache = %v, want empty", got)
	}
}

func TestCachedEndStatesMakeTheFirstFrameAccurate(t *testing.T) {
	a := testApp(t)
	for _, key := range []string{"acme/api#1", "acme/api#2"} {
		record(t, a, key, func(r *state.Record) {
			r.Title = "reviewed"
			r.Mark(state.OK, "")
		})
	}
	if err := writeEndStateCache(a.p.PRStatesFile, map[string]string{
		"acme/api#1": gh.StateMerged,
		"acme/api#2": gh.StateOpen,
	}); err != nil {
		t.Fatal(err)
	}

	ends := newEndStates(a.p.PRStatesFile)
	screen, tracked := render(t, a, ends.snapshot())
	open := sectionOf(t, screen, "OPEN")
	if strings.Contains(open, "api #1") || !strings.Contains(open, "api #2") {
		t.Fatalf("cached first frame has the wrong OPEN section:\n%s", screen)
	}
	for _, key := range []string{"acme/api#1", "acme/api#2"} {
		if !slices.Contains(tracked, key) {
			t.Errorf("tracked = %v, want cached candidate %s refreshed", tracked, key)
		}
	}
}

func TestRefreshEndStatesPersistsSuccessAndKeepsCacheOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-states.json")
	ends := newEndStates(path)
	keys := []string{"acme/api#1", "acme/api#2"}

	good := fakeEndStateGH(t, `echo '{"data":{"p0":{"pullRequest":{"state":"MERGED"}},"p1":{"pullRequest":{"state":"OPEN"}}}}'`)
	if err := refreshEndStates(context.Background(), gh.New(good), ends, keys); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"acme/api#1": gh.StateMerged, "acme/api#2": gh.StateOpen}
	if got := readEndStateCache(path); len(got) != 2 || got["acme/api#1"] != want["acme/api#1"] || got["acme/api#2"] != want["acme/api#2"] {
		t.Fatalf("persisted cache = %v, want %v", got, want)
	}

	bad := fakeEndStateGH(t, `echo 'network down' >&2; exit 1`)
	client := gh.New(bad)
	client.Attempts = 1
	if err := refreshEndStates(context.Background(), client, ends, keys); err == nil {
		t.Fatal("failed refresh returned nil")
	}
	if got := ends.snapshot(); len(got) != 2 || got["acme/api#1"] != gh.StateMerged || got["acme/api#2"] != gh.StateOpen {
		t.Fatalf("failed refresh replaced cached states: %v", got)
	}
}

func fakeEndStateGH(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

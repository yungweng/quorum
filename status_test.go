package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

// testApp builds an app whose whole world is one temporary directory.
func testApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	p := paths.P{
		StateDir:   dir,
		StateFile:  filepath.Join(dir, "state.json"),
		Log:        filepath.Join(dir, "log"),
		RunningDir: filepath.Join(dir, "running"),
		ReviewRuns: filepath.Join(dir, "runs"),
	}
	cfg := config.Config{MaxConcurrent: 6, Reviewers: 6, PollInterval: 120}
	return &app{cfg: cfg, p: p, log: logbook.New(p.Log)}
}

// render draws the dashboard onto a terminal-capable writer and returns both
// the screen and the pull requests it says it drew.
func render(t *testing.T, a *app, ends map[string]string) (string, []string) {
	t.Helper()
	var b strings.Builder
	w := &ui.Writer{Out: &b, Color: true, Links: true, Width: 100}
	shown := a.dashboard(w, ends)
	return b.String(), shown
}

func record(t *testing.T, a *app, key string, fn func(*state.Record)) {
	t.Helper()
	if err := state.Mutate(a.p.StateFile, key, fn); err != nil {
		t.Fatal(err)
	}
}

// A section that vanishes when it is empty cannot be told apart from a feature
// that is not there. Both stages of the pipeline have to stay on screen and say
// that they are idle.
func TestDashboardKeepsThePipelineVisibleWhenIdle(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#1", func(r *state.Record) {
		r.Title = "a finished one"
		r.Mark(state.OK, "")
	})

	screen, _ := render(t, a, nil)
	for _, want := range []string{"RUNNING", "nothing under review right now", "QUEUED", "nothing waiting"} {
		if !strings.Contains(screen, want) {
			t.Errorf("idle dashboard is missing %q:\n%s", want, screen)
		}
	}
}

func TestDashboardShowsWhatIsRunningAndQueued(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#1", func(r *state.Record) {
		r.Title = "under review"
		r.Mark(state.Running, "")
	})
	record(t, a, "acme/api#2", func(r *state.Record) {
		r.Title = "waiting its turn"
		r.Mark(state.Pending, "waiting for a free slot")
	})

	screen, shown := render(t, a, nil)
	if strings.Contains(screen, "nothing under review right now") {
		t.Errorf("a running review was reported as idle:\n%s", screen)
	}
	for _, want := range []string{"api #1", "under review", "api #2", "waiting for a free slot"} {
		if !strings.Contains(screen, want) {
			t.Errorf("dashboard is missing %q:\n%s", want, screen)
		}
	}
	// watch looks up exactly what is on screen, so the drawn keys have to be
	// reported back or the lookup asks about the wrong pull requests.
	if len(shown) != 2 || !contains(shown, "acme/api#1") || !contains(shown, "acme/api#2") {
		t.Errorf("dashboard reported %v as drawn", shown)
	}
}

// Merged is struck through, because the work landed and the line is history.
// The word is printed as well: a terminal that ignores SGR 9 would otherwise
// show nothing at all.
func TestDashboardCrossesOutMergedPullRequests(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#1", func(r *state.Record) {
		r.Title = "landed"
		r.Mark(state.OK, "")
	})
	record(t, a, "acme/api#2", func(r *state.Record) {
		r.Title = "still going"
		r.Mark(state.OK, "")
	})
	record(t, a, "acme/api#3", func(r *state.Record) {
		r.Title = "abandoned"
		r.Mark(state.OK, "")
	})

	screen, _ := render(t, a, map[string]string{
		"acme/api#1": gh.StateMerged,
		"acme/api#2": gh.StateOpen,
		"acme/api#3": gh.StateClosed,
	})

	merged := lineWith(t, screen, "api #1")
	if !strings.Contains(merged, "\x1b[9m") {
		t.Errorf("merged pull request was not struck out: %q", merged)
	}
	if !strings.Contains(merged, "merged") {
		t.Errorf("merged pull request carried no word for terminals without SGR 9: %q", merged)
	}

	open := lineWith(t, screen, "api #2")
	if strings.Contains(open, "\x1b[9m") {
		t.Errorf("an open pull request was struck out: %q", open)
	}
	if strings.Contains(open, "merged") {
		t.Errorf("an open pull request was labelled merged: %q", open)
	}

	// Closed without merging is not a success and must not read like one.
	closed := lineWith(t, screen, "api #3")
	if strings.Contains(closed, "\x1b[9m") {
		t.Errorf("a closed pull request was struck out like a merged one: %q", closed)
	}
	if !strings.Contains(closed, "closed unmerged") {
		t.Errorf("a closed pull request was not marked: %q", closed)
	}
}

// Nothing may depend on the lookup having happened: watch draws its first frame
// before GitHub has answered, and the answer never arrives when gh is missing.
func TestDashboardRendersWithoutAnyMergeInformation(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#1", func(r *state.Record) {
		r.Title = "landed"
		r.Mark(state.OK, "")
	})
	screen, _ := render(t, a, nil)
	if strings.Contains(screen, "\x1b[9m") {
		t.Errorf("something was struck out without a known state:\n%s", screen)
	}
	if !strings.Contains(screen, "api #1") {
		t.Errorf("the pull request went missing:\n%s", screen)
	}
}

// Every line has to fit the terminal: watch paints a fixed number of rows, and
// a line that wraps pushes the rest of the frame off the bottom.
func TestDashboardLinesFitTheTerminal(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#1", func(r *state.Record) {
		r.Title = strings.Repeat("a very long german pull request title ", 6)
		r.Mark(state.Running, "")
	})
	record(t, a, "acme/api#2", func(r *state.Record) {
		r.Title = strings.Repeat("another long one ", 8)
		r.Mark(state.Pending, "waiting for a free slot")
	})

	const width = 100
	var b strings.Builder
	w := &ui.Writer{Out: &b, Width: width}
	a.dashboard(w, map[string]string{"acme/api#1": gh.StateClosed, "acme/api#2": gh.StateMerged})
	for _, line := range strings.Split(b.String(), "\n") {
		if len([]rune(line)) > width {
			t.Errorf("line is %d cells wide, terminal is %d: %q", len([]rune(line)), width, line)
		}
	}
}

// lineWith returns the single screen line containing want.
func lineWith(t *testing.T, screen, want string) string {
	t.Helper()
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no line contains %q:\n%s", want, screen)
	return ""
}

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

// testApp builds an app whose whole world is one temporary directory.
func testApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	p := paths.P{
		StateDir:    dir,
		StateFile:   filepath.Join(dir, "state.json"),
		Log:         filepath.Join(dir, "log"),
		RunningDir:  filepath.Join(dir, "running"),
		ReviewRuns:  filepath.Join(dir, "runs"),
		BabysitRuns: filepath.Join(dir, "babysit"),
		DepsCache:   filepath.Join(dir, "deps"),
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

// babysit fakes a fix loop in flight the way a real one appears on disk: a run
// directory holding the claim, with a progress file inside it.
func babysit(t *testing.T, a *app, p loop.Progress) {
	t.Helper()
	dir := filepath.Join(a.p.BabysitRuns, fmt.Sprintf("run-%d", p.Number))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := proc.Claim(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, loop.ProgressFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// marker fakes the claim the agent takes on a review slot.
func marker(t *testing.T, a *app, key string, pid int) {
	t.Helper()
	m, got, err := runner.Acquire(a.p.RunningDir, key)
	if err != nil || !got {
		t.Fatalf("could not claim %s: %v", key, err)
	}
	t.Cleanup(m.Release)
	if err := m.Adopt(pid); err != nil {
		t.Fatal(err)
	}
}

// A section that vanishes when it is empty cannot be told apart from a feature
// that is not there. Every stage of the pipeline has to stay on screen and say
// that it is idle.
func TestDashboardKeepsThePipelineVisibleWhenIdle(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#1", func(r *state.Record) {
		r.Title = "a finished one"
		r.Mark(state.OK, "")
	})

	screen, _ := render(t, a, nil)
	for _, want := range []string{
		"REVIEWING", "nothing under review right now",
		"BABYSITTING", "no fix loop running",
		"QUEUED", "nothing waiting",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("idle dashboard is missing %q:\n%s", want, screen)
		}
	}
}

// Cache size is linear in the number of cached files. The dashboard is an
// interactive status path, so it must not start that walk merely to draw a
// system summary.
func TestDashboardDoesNotMeasureCache(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = 4
	if err := os.MkdirAll(a.p.DepsCache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.p.DepsCache, "tree"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	screen, _ := render(t, a, nil)

	if !a.cacheAt.IsZero() {
		t.Fatal("dashboard measured the cache")
	}
	if !strings.Contains(screen, "4.0 GB budget") {
		t.Fatalf("dashboard is missing the cache budget:\n%s", screen)
	}
}

// A fix loop started from a terminal never touches the state file and never
// takes a review slot. The run cache is the only place it exists, so a
// dashboard that only reads the state file shows nothing while one runs for
// hours.
func TestDashboardShowsATerminalBabysit(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	babysit(t, a, loop.Progress{
		PID: 999999, Repo: "acme/api", Number: 42, Title: "tenant scoping",
		StartedAt: now.Add(-90 * time.Minute), MaxIter: 12, MaxCIFixes: 3,
		Round: 3, Phase: loop.PhaseFix, Since: now.Add(-18 * time.Minute),
		CI: loop.CIGreen, Reviewed: true, Blockers: 1, Critical: 2, Commits: 5,
	})

	screen, shown := render(t, a, map[string]string{"acme/api#42": gh.StateMerged})
	if strings.Contains(screen, "no fix loop running") {
		t.Errorf("a running fix loop was reported as idle:\n%s", screen)
	}
	for _, want := range []string{
		"api #42", "tenant scoping", // which pull request
		"1h30m",      // how long it has been going
		"round 3/12", // where in the loop
		"CI ✓",       // what the checks said
		"review 1B 2C",
		"fixing ● 18m", // what it is doing now, and since when
		"5 commits",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("babysit line is missing %q:\n%s", want, screen)
		}
	}
	// It must not be counted against the review budget: babysit takes no slot,
	// so a count next to the heading would be a budget that does not exist.
	if strings.Contains(screen, "BABYSITTING  1 of") {
		t.Errorf("the fix loop was counted against the review slots:\n%s", screen)
	}
	if got := lineWith(t, screen, "api #42"); !strings.Contains(got, "merged") {
		t.Errorf("the merged fix loop was not labelled: %q", got)
	}
	if len(shown) != 1 || shown[0] != "acme/api#42" {
		t.Errorf("shown = %v, want the fix loop key for end-state tracking", shown)
	}
}

// A fix loop the agent started has a state record too. Drawing both is the same
// pull request twice, and the review line is the less informative of the two.
func TestDashboardDoesNotShowAnAgentBabysitTwice(t *testing.T) {
	a := testApp(t)
	pid := os.Getpid()
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "tenant scoping"
		r.Mark(state.Running, "")
	})
	marker(t, a, "acme/api#42", pid)
	babysit(t, a, loop.Progress{
		PID: pid, Repo: "acme/api", Number: 42, Title: "tenant scoping",
		StartedAt: time.Now().Add(-5 * time.Minute), MaxIter: 12,
		Phase: loop.PhaseCI, Since: time.Now().Add(-5 * time.Minute),
	})

	screen, _ := render(t, a, nil)
	if !strings.Contains(screen, "nothing under review right now") {
		t.Errorf("the agent's fix loop was drawn as a review as well:\n%s", screen)
	}
	if !strings.Contains(screen, "waiting for CI") {
		t.Errorf("the agent's fix loop is missing from BABYSITTING:\n%s", screen)
	}
	if !strings.Contains(screen, "REVIEWING  1 of 6") {
		t.Errorf("the agent's fix loop is missing from the scheduler capacity count:\n%s", screen)
	}
}

func TestDashboardShowsADirectRunBeforeItsStateUpdate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{name: "without a record"},
		{name: "with a stale record", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			if tc.stale {
				record(t, a, "acme/api#42", func(r *state.Record) {
					r.Title = "an earlier review"
					r.Mark(state.OK, "")
				})
			}
			marker(t, a, "acme/api#42", os.Getpid())

			screen, shown := render(t, a, nil)
			if strings.Contains(screen, "nothing under review right now") {
				t.Errorf("a claimed direct review was reported as idle:\n%s", screen)
			}
			for _, want := range []string{"REVIEWING  1 of 6", "api #42", "starting up"} {
				if !strings.Contains(screen, want) {
					t.Errorf("preparing direct review is missing %q:\n%s", want, screen)
				}
			}
			if strings.Count(screen, "api #42") != 1 {
				t.Errorf("the direct review was not deduplicated:\n%s", screen)
			}
			if len(shown) != 1 || shown[0] != "acme/api#42" {
				t.Errorf("shown = %v, want the direct review key", shown)
			}
		})
	}
}

// Matching on the pull request alone would hide a genuine review whenever
// somebody happens to babysit the same one from a terminal. The marker names
// the process the pipeline runs in, which is what tells the two apart.
func TestDashboardKeepsAReviewOfAPullRequestSomebodyElseIsBabysitting(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "tenant scoping"
		r.Mark(state.Running, "")
	})
	marker(t, a, "acme/api#42", os.Getpid())
	babysit(t, a, loop.Progress{
		PID: 999999, Repo: "acme/api", Number: 42, Title: "tenant scoping",
		StartedAt: time.Now().Add(-5 * time.Minute), MaxIter: 12,
		Phase: loop.PhaseFix, Since: time.Now().Add(-time.Minute),
	})

	screen, _ := render(t, a, nil)
	if strings.Contains(screen, "nothing under review right now") {
		t.Errorf("an unrelated review was hidden by a terminal babysit:\n%s", screen)
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
	if len(shown) != 2 || !slices.Contains(shown, "acme/api#1") || !slices.Contains(shown, "acme/api#2") {
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
	longKey := "acme/" + strings.Repeat("r", 100) + "#1"
	record(t, a, longKey, func(r *state.Record) {
		r.Title = strings.Repeat("a very long german pull request title ", 6)
		r.Mark(state.Running, "")
	})
	record(t, a, "acme/api#2", func(r *state.Record) {
		r.Title = strings.Repeat("another long one ", 8)
		r.Mark(state.Pending, "waiting for a free slot")
	})
	// The widest a fix loop line gets: every segment present, every number two
	// digits, and a title that would run off the screen on its own.
	now := time.Now()
	babysit(t, a, loop.Progress{
		PID: 999999, Repo: "acme/api", Number: 3,
		Title:     strings.Repeat("ein sehr langer deutscher Titel ", 6),
		StartedAt: now.Add(-25 * time.Hour), MaxIter: 12, MaxCIFixes: 3,
		Round: 12, Phase: loop.PhaseCIFix, Since: now.Add(-25 * time.Hour),
		CI: loop.CIRed, CIFix: 3, Reviewed: true, Blockers: 12, Critical: 12,
		Commits: 99,
	})

	for _, width := range []int{80, 100} {
		var b strings.Builder
		w := &ui.Writer{Out: &b, Width: width}
		a.dashboard(w, map[string]string{longKey: gh.StateClosed, "acme/api#2": gh.StateMerged})
		for _, line := range strings.Split(b.String(), "\n") {
			if len([]rune(line)) > width {
				t.Errorf("line is %d cells wide, terminal is %d: %q", len([]rune(line)), width, line)
			}
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

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
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/review"
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
		HistoryFile: filepath.Join(dir, "history.jsonl"),
		Log:         filepath.Join(dir, "log"),
		RunningDir:  filepath.Join(dir, "running"),
		ManualDir:   filepath.Join(dir, "manual-reviews"),
		ReviewRuns:  filepath.Join(dir, "runs"),
		BabysitRuns: filepath.Join(dir, "babysit"),
		DepsCache:   filepath.Join(dir, "deps"),
	}
	cfg := config.Config{MaxConcurrent: 6, Reviewers: 6, PollInterval: 120, History: 20}
	return &app{cfg: cfg, p: p, log: logbook.New(p.Log)}
}

func TestRecentReviewedPRKeysIncludesEveryEligibleRecord(t *testing.T) {
	now := time.Now()
	file := state.File{PRs: map[string]state.Record{}}
	for i := 1; i <= 25; i++ {
		file.PRs[fmt.Sprintf("acme/api#%d", i)] = state.Record{
			Status: state.OK,
			At:     now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
		}
	}

	keys := recentReviewedPRKeys(file, now)
	if len(keys) != 25 {
		t.Fatalf("recent reviewed PR keys = %d, want 25", len(keys))
	}
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

// manualReview fakes the small live file written by `quorum review`.
func manualReview(t *testing.T, a *app, p review.LiveRun, events string) {
	t.Helper()
	dir := filepath.Join(a.p.ReviewRuns, fmt.Sprintf("run-%d", p.Number))
	if err := os.MkdirAll(filepath.Join(dir, "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := proc.Claim(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	p.RunDir = dir
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.p.ManualDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.p.ManualDir, fmt.Sprintf("%d.json", p.PID)), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if events != "" {
		if err := os.WriteFile(filepath.Join(dir, "output", "events.log"), []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
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

func TestDashboardShowsAManualReviewWithoutTakingAnAgentSlot(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "an earlier review"
		r.Mark(state.OK, "")
	})
	manualReview(t, a, review.LiveRun{
		PID: os.Getpid(), Repo: "acme/api", Number: 42, Title: "tenant scoping",
		StartedAt: time.Now().Add(-2 * time.Minute), Reviewers: 2,
	}, "start\nok 1 30 1\n")

	screen, shown := render(t, a, nil)
	if got := sectionBadge(t, screen, "ACTIVE"); got != "0 / 6" {
		t.Errorf("a manual review took an agent slot: heading says %q", got)
	}
	for _, want := range []string{
		"api #42", "tenant scoping",
		"manual", "2m", "1/2 reviewers done",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("manual review is missing %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "nothing under review right now") {
		t.Errorf("a manual review was reported as idle:\n%s", screen)
	}
	if strings.Contains(screen, "an earlier review") {
		t.Errorf("the superseded recent result remained visible:\n%s", screen)
	}
	if strings.Contains(screen, "agent ·") {
		t.Errorf("the manual review was labelled as an agent run:\n%s", screen)
	}
	if len(shown) != 1 || shown[0] != "acme/api#42" {
		t.Errorf("shown = %v, want one manual review key", shown)
	}
}

// The open section is the answer to "what is waiting for me": a review that
// found something stays in view until the pull request is merged or closed,
// instead of scrolling away under later runs.
func TestOpenSectionListsReviewedPullRequestsThatAreStillOpen(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "tenant scoping"
		r.Critical = 2
		r.CommentURL = "https://github.com/acme/api/pull/42#issuecomment-1"
		r.Mark(state.OK, "")
	})

	screen, shown := render(t, a, nil)
	open := sectionOf(t, screen, "OPEN")
	for _, want := range []string{"api #42", "tenant scoping", "0B 2C 0S", "comment"} {
		if !strings.Contains(open, want) {
			t.Errorf("the open section is missing %q:\n%s", want, screen)
		}
	}
	if !slices.Contains(shown, "acme/api#42") {
		t.Errorf("shown = %v, an open pull request must be looked up", shown)
	}
}

// Once it is merged or closed there is nothing left to do about it, and the
// history section already records that it happened.
func TestOpenSectionDropsMergedAndClosedPullRequests(t *testing.T) {
	a := testApp(t)
	for _, key := range []string{"acme/api#42", "acme/api#43", "acme/api#44"} {
		record(t, a, key, func(r *state.Record) { r.Mark(state.OK, "") })
	}
	ends := map[string]string{
		"acme/api#42": gh.StateMerged,
		"acme/api#43": gh.StateClosed,
		"acme/api#44": gh.StateOpen,
	}

	screen, _ := render(t, a, ends)
	open := sectionOf(t, screen, "OPEN")
	if strings.Contains(open, "api #42") || strings.Contains(open, "api #43") {
		t.Errorf("a merged or closed pull request stayed in the open section:\n%s", screen)
	}
	if !strings.Contains(open, "api #44") {
		t.Errorf("the open pull request is missing:\n%s", screen)
	}
}

// A pull request being reviewed right now belongs under ACTIVE. Listing the
// result of the previous run above it as well says the same pull request twice.
func TestOpenSectionLeavesOutWhatIsAlreadyActive(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "an earlier review"
		r.Mark(state.OK, "")
	})
	manualReview(t, a, review.LiveRun{
		PID: os.Getpid(), Repo: "acme/api", Number: 42, Title: "an earlier review",
		StartedAt: time.Now(), Reviewers: 2,
	}, "")

	screen, _ := render(t, a, nil)
	if got := strings.Count(ui.StripANSI(screen), "api #42"); got != 1 {
		t.Errorf("api #42 is on screen %d times, want once:\n%s", got, screen)
	}
}

// Two hundred records outlive the pull requests they describe. Age is the only
// thing the dashboard knows about one GitHub has not been asked about, so an
// old one is history rather than something still waiting for a person.
func TestOpenSectionStopsAtTheWindowAndTheLimit(t *testing.T) {
	a := testApp(t)
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	record(t, a, "acme/api#1", func(r *state.Record) {
		r.Mark(state.OK, "")
		r.At = old
	})
	for i := range openLimit + 5 {
		record(t, a, fmt.Sprintf("acme/web#%d", i+1), func(r *state.Record) { r.Mark(state.OK, "") })
	}

	screen, _ := render(t, a, nil)
	open := sectionOf(t, screen, "OPEN")
	if strings.Contains(open, "api #1") {
		t.Errorf("a month old review is still listed as open:\n%s", screen)
	}
	if got := strings.Count(open, "web #"); got != openLimit {
		t.Errorf("the open section lists %d pull requests, want %d:\n%s", got, openLimit, screen)
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
	for _, want := range []string{"ACTIVE", "nothing running", "HISTORY"} {
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
	if !strings.Contains(screen, "4.0 GB cache") {
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
	if got := sectionBadge(t, screen, "ACTIVE"); got != "0 / 6" {
		t.Errorf("a terminal fix loop was counted against the review slots: heading says %q", got)
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
	if strings.Count(screen, "api #42") != 1 {
		t.Errorf("the agent's fix loop was drawn as a review as well:\n%s", screen)
	}
	if !strings.Contains(screen, "waiting for CI") {
		t.Errorf("the agent's fix loop is missing from ACTIVE:\n%s", screen)
	}
	if got := sectionBadge(t, screen, "ACTIVE"); got != "1 / 6" {
		t.Errorf("the agent's fix loop is missing from the scheduler capacity count: heading says %q", got)
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
			if got := sectionBadge(t, screen, "ACTIVE"); got != "1 / 6" {
				t.Errorf("preparing direct review is not counted: heading says %q", got)
			}
			for _, want := range []string{"api #42", "agent", "starting up"} {
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
// The point of the history log: runs are listed one by one, so reviewing the
// same pull request twice leaves two lines instead of the second overwriting
// the first, and a run started in a terminal appears at all.
func TestDashboardListsEveryRunNotEveryPullRequest(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	for _, run := range []history.Run{
		{Key: "acme/api#42", Kind: history.KindReview, Source: history.SourceAgent,
			Outcome: history.OK, Reviewed: true, Critical: 2, EndedAt: now.Add(-2 * time.Hour)},
		{Key: "acme/api#42", Kind: history.KindBabysit, Source: history.SourceManual,
			Outcome: history.Converged, Reviewed: true, Rounds: 3, EndedAt: now.Add(-time.Hour)},
		{Key: "acme/web#7", Kind: history.KindReview, Source: history.SourceManual,
			Outcome: history.Failed, Reason: "reviewer-2 timed out", EndedAt: now.Add(-time.Minute)},
	} {
		if err := history.Append(a.p.HistoryFile, run); err != nil {
			t.Fatal(err)
		}
	}

	screen, shown := render(t, a, nil)
	if got := strings.Count(screen, "api #42"); got != 2 {
		t.Errorf("two runs on one pull request produced %d lines:\n%s", got, screen)
	}
	// Newest first.
	if strings.Index(screen, "web #7") > strings.Index(screen, "api #42") {
		t.Errorf("the history is not newest first:\n%s", screen)
	}
	for _, want := range []string{"fix, 3 rounds", "0B 2C 0S", "reviewer-2 timed out"} {
		if !strings.Contains(screen, want) {
			t.Errorf("history is missing %q:\n%s", want, screen)
		}
	}
	if !slices.Contains(shown, "acme/web#7") {
		t.Errorf("shown = %v, want the logged runs for end-state tracking", shown)
	}
}

func TestDashboardRendersBranchHistoryWithoutTrackingItAsAPR(t *testing.T) {
	a := testApp(t)
	run := history.Run{
		Key:      history.BranchKey("acme/api", "feature/crumb-tray"),
		Branch:   "feature/crumb-tray",
		Kind:     history.KindReview,
		Source:   history.SourceManual,
		Outcome:  history.OK,
		Reviewed: true,
		EndedAt:  time.Now(),
	}
	if err := history.Append(a.p.HistoryFile, run); err != nil {
		t.Fatal(err)
	}

	screen, tracked := render(t, a, nil)
	if !strings.Contains(screen, "api feature/crumb-tray") || strings.Contains(screen, "api #0") {
		t.Fatalf("branch history was not rendered as a branch:\n%s", screen)
	}
	if slices.Contains(tracked, run.Key) {
		t.Fatalf("branch key %q was sent for PR end-state tracking", run.Key)
	}
}

func TestDashboardHonoursTheConfiguredHistorySize(t *testing.T) {
	a := testApp(t)
	a.cfg.History = 3
	for i := range 10 {
		if err := history.Append(a.p.HistoryFile, history.Run{
			Key: fmt.Sprintf("acme/api#%d", i), Kind: history.KindReview,
			Outcome: history.OK, Reviewed: true, EndedAt: time.Now().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	screen, _ := render(t, a, nil)
	if got := strings.Count(screen, "api #"); got != 3 {
		t.Errorf("HISTORY=3 listed %d runs:\n%s", got, screen)
	}
	// The newest three, not the oldest.
	if !strings.Contains(screen, "api #9") || strings.Contains(screen, "api #0") {
		t.Errorf("the wrong end of the log was listed:\n%s", screen)
	}
}

// Upgrading to a build that keeps a log must not show an empty screen for
// everything the agent did before it existed.
func TestDashboardFallsBackToTheStateFileWhileTheLogIsEmpty(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "an older result"
		r.Critical = 1
		r.Mark(state.OK, "")
	})

	screen, _ := render(t, a, nil)
	if !strings.Contains(screen, "api #42") {
		t.Errorf("the state file's results vanished with an empty log:\n%s", screen)
	}

	// The first logged run takes over.
	if err := history.Append(a.p.HistoryFile, history.Run{
		Key: "acme/web#7", Kind: history.KindReview, Outcome: history.OK,
		Reviewed: true, EndedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	screen, _ = render(t, a, nil)
	if !strings.Contains(screen, "web #7") {
		t.Errorf("the logged run is missing:\n%s", screen)
	}
	// Only the history section stops using the fallback. The record itself is
	// a reviewed pull request that is still open, which is what the open
	// section above is for, so it stays on screen there.
	if history := sectionOf(t, screen, "HISTORY"); strings.Contains(history, "api #42") {
		t.Errorf("the fallback was still used once the log had an entry:\n%s", screen)
	}
	if open := sectionOf(t, screen, "OPEN"); !strings.Contains(open, "api #42") {
		t.Errorf("the reviewed pull request left the open section:\n%s", screen)
	}
}

func TestDashboardSaysSoWhenNothingHasFinished(t *testing.T) {
	screen, _ := render(t, testApp(t), nil)
	if !strings.Contains(screen, "nothing has finished yet") {
		t.Errorf("an empty history does not say so:\n%s", screen)
	}
}

// An idle machine is what this screen shows almost all of the time, so the
// idle state has to be compact rather than four headings saying "nothing".
func TestAnIdleDashboardStaysCompact(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = 5

	var b strings.Builder
	a.dashboard(&ui.Writer{Out: &b, Width: 120}, nil)
	screen := b.String()

	// It still says in words that nothing is running: a section that vanishes
	// cannot be told apart from a feature that is not there.
	if !strings.Contains(lineWith(t, screen, "ACTIVE"), "nothing running") {
		t.Errorf("the idle state is not stated on the ACTIVE line:\n%s", screen)
	}
	if !strings.Contains(lineWith(t, screen, "OPEN"), "nothing reviewed is still open") {
		t.Errorf("the idle state is not stated on the OPEN line:\n%s", screen)
	}
	// And it says it once, not once per pipeline stage.
	for _, gone := range []string{"REVIEWING", "BABYSITTING", "QUEUED", "SYSTEM"} {
		if strings.Contains(screen, gone) {
			t.Errorf("%s still has a heading of its own:\n%s", gone, screen)
		}
	}
	// Three sections, each stating its own idle case in one line, plus header,
	// status bar and footer.
	if n := strings.Count(strings.TrimSpace(screen), "\n") + 1; n > 13 {
		t.Errorf("an idle dashboard is %d lines:\n%s", n, screen)
	}
	// The settings did not disappear with the section that held them.
	if !strings.Contains(screen, "5.0 GB cache") {
		t.Errorf("the footer is missing the cache budget:\n%s", screen)
	}
}

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

	// A title whose runes and columns disagree, which is what used to push
	// every column to its right out past the edge of the screen.
	record(t, a, "acme/web#4", func(r *state.Record) {
		r.Title = "✨ " + strings.Repeat("日本語のタイトル ", 8)
		r.Mark(state.Pending, "waiting")
	})

	// Both sides of the two column threshold, and a terminal wider than the
	// layout cap, where lines must stop at the cap rather than at the edge.
	for _, width := range []int{80, 100, 112, 140, 220} {
		var b strings.Builder
		w := &ui.Writer{Out: &b, Width: width}
		a.dashboard(w, map[string]string{longKey: gh.StateClosed, "acme/api#2": gh.StateMerged})
		limit := min(width, ui.MaxWidth)
		for _, line := range strings.Split(b.String(), "\n") {
			if ui.Cells(line) > limit {
				t.Errorf("line is %d columns wide, budget is %d: %q", ui.Cells(line), limit, line)
			}
		}
	}
}

// lineWith returns the single screen line containing want.
// sectionBadge returns the count a section heading carries at its right edge,
// or "" when it carries none. The gap in between depends on the terminal
// width, so the badge has to be read off the line rather than matched as one
// literal string.
func sectionBadge(t *testing.T, screen, heading string) string {
	t.Helper()
	for _, line := range strings.Split(ui.StripANSI(screen), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), heading)
		if !ok {
			continue
		}
		return strings.TrimSpace(rest)
	}
	t.Fatalf("no %s heading:\n%s", heading, screen)
	return ""
}

// sectionOf returns one section of the dashboard, from its heading to the next
// blank line that starts a new one. Sections show the same pull request for
// different reasons, so an assertion about one of them has to say which.
func sectionOf(t *testing.T, screen, heading string) string {
	t.Helper()
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		if !strings.Contains(line, heading) {
			continue
		}
		out := []string{line}
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) == "" {
				break
			}
			out = append(out, next)
		}
		return strings.Join(out, "\n")
	}
	t.Fatalf("no %s section:\n%s", heading, screen)
	return ""
}

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

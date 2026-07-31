package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
		StateDir:     dir,
		StateFile:    filepath.Join(dir, "state.json"),
		PRStatesFile: filepath.Join(dir, "pr-states.json"),
		HistoryFile:  filepath.Join(dir, "history.jsonl"),
		Log:          filepath.Join(dir, "log"),
		RunningDir:   filepath.Join(dir, "running"),
		ManualDir:    filepath.Join(dir, "manual-reviews"),
		ReviewRuns:   filepath.Join(dir, "runs"),
		BabysitRuns:  filepath.Join(dir, "babysit"),
		DepsCache:    filepath.Join(dir, "deps"),
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

	keys := recentReviewedPRKeys(reviewedPRs(file, nil), now)
	if len(keys) != 25 {
		t.Fatalf("recent reviewed PR keys = %d, want 25", len(keys))
	}
}

func TestOpenSectionIncludesSuccessfulManualRuns(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	for _, run := range []history.Run{
		{
			Key: "acme/api#42", Title: "manual review", Kind: history.KindReview,
			Source: history.SourceManual, Outcome: history.OK, Reviewed: true,
			Blockers: 2, Critical: 1, CommentURL: "https://example.invalid/comment/42", EndedAt: now,
		},
		{
			Key: "acme/web#43", Title: "manual fix loop", Kind: history.KindBabysit,
			Source: history.SourceManual, Outcome: history.Converged, Reviewed: true, EndedAt: now.Add(-time.Minute),
		},
	} {
		if err := history.Append(a.p.HistoryFile, run); err != nil {
			t.Fatal(err)
		}
	}

	screen, tracked := render(t, a, map[string]string{
		"acme/api#42": gh.StateOpen,
		"acme/web#43": gh.StateOpen,
	})
	open := sectionOf(t, screen, "OPEN")
	for _, want := range []string{"api #42", "manual review", "2B 1C 0S", "web #43", "manual fix loop"} {
		if !strings.Contains(open, want) {
			t.Errorf("OPEN is missing %q:\n%s", want, screen)
		}
	}
	for _, key := range []string{"acme/api#42", "acme/web#43"} {
		if !slices.Contains(tracked, key) {
			t.Errorf("tracked = %v, want manual run %s", tracked, key)
		}
	}
}

func TestOpenSectionKeepsManualResultsWhenMergeFails(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	for _, run := range []history.Run{
		{
			Key: "acme/api#42", Title: "review result", Kind: history.KindReview,
			Source: history.SourceManual, Outcome: history.OK, Reviewed: true,
			Reason: "auto-merge failed: permission denied", EndedAt: now,
		},
		{
			Key: "acme/web#43", Title: "fix-loop result", Kind: history.KindBabysit,
			Source: history.SourceManual, Outcome: history.Converged, Reviewed: true,
			Reason: "auto-merge failed: permission denied", EndedAt: now.Add(-time.Minute),
		},
	} {
		if err := history.Append(a.p.HistoryFile, run); err != nil {
			t.Fatal(err)
		}
	}

	screen, _ := render(t, a, map[string]string{
		"acme/api#42": gh.StateOpen,
		"acme/web#43": gh.StateOpen,
	})
	open := sectionOf(t, screen, "OPEN")
	for _, want := range []string{"api #42", "review result", "web #43", "fix-loop result", "merge failed", "permission denied"} {
		if !strings.Contains(open, want) {
			t.Errorf("OPEN is missing %q after a merge failure:\n%s", want, screen)
		}
	}
	if historySection := sectionOf(t, screen, "HISTORY"); !strings.Contains(historySection, "merge failed") {
		t.Errorf("HISTORY hides the merge failure:\n%s", screen)
	}
}

func TestDashboardSurfacesRecordedMergeFailure(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "review result"
		r.Status = state.OK
		r.Reason = "auto-merge failed: permission denied"
		r.At = time.Now().Format(time.RFC3339)
	})

	screen, _ := render(t, a, map[string]string{"acme/api#42": gh.StateOpen})
	for _, section := range []string{"OPEN", "HISTORY"} {
		text := sectionOf(t, screen, section)
		for _, want := range []string{"merge failed", "permission denied"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing %q:\n%s", section, want, screen)
			}
		}
	}
}

func TestNewerFailedManualRunHidesStaleReview(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	for _, run := range []history.Run{
		{Key: "acme/api#42", Source: history.SourceManual, Outcome: history.OK, Reviewed: true, EndedAt: now.Add(-time.Hour)},
		{Key: "acme/api#42", Source: history.SourceManual, Outcome: history.Failed, EndedAt: now},
	} {
		if err := history.Append(a.p.HistoryFile, run); err != nil {
			t.Fatal(err)
		}
	}

	screen, _ := render(t, a, nil)
	if open := sectionOf(t, screen, "OPEN"); strings.Contains(open, "api #42") {
		t.Errorf("OPEN kept a stale successful review after a newer failure:\n%s", screen)
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

	screen, shown := render(t, a, map[string]string{"acme/api#42": gh.StateOpen})
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

	screen, shown := render(t, a, map[string]string{"acme/api#42": gh.StateOpen})
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

func TestOpenSectionKeepsReviewedPullRequestAfterMergeFailure(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/api#42", func(r *state.Record) {
		r.Title = "protected merge"
		r.CommentURL = "https://example.invalid/comment/42"
		r.Suggestions = 1
		r.Mark(state.OK, "auto-merge failed: permission denied")
	})

	screen, _ := render(t, a, map[string]string{"acme/api#42": gh.StateOpen})
	open := sectionOf(t, screen, "OPEN")
	for _, want := range []string{"api #42", "protected merge", "0B 0C 1S", "comment"} {
		if !strings.Contains(open, want) {
			t.Errorf("OPEN is missing %q after a merge failure:\n%s", want, screen)
		}
	}
}

func TestDashboardShowsPRAuthorInEverySection(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/open#42", func(r *state.Record) {
		r.Title = "open review"
		r.Author = "open-author"
		r.Mark(state.OK, "")
	})
	manualReview(t, a, review.LiveRun{
		PID: os.Getpid(), Repo: "acme/active", Number: 43, Title: "active review",
		Author: "active-author", StartedAt: time.Now(), Reviewers: 2,
	}, "start\n")
	if err := history.Append(a.p.HistoryFile, history.Run{
		Key: "acme/history#44", Title: "finished review", Author: "history-author",
		Kind: history.KindReview, Source: history.SourceManual, Outcome: history.OK,
		Reviewed: true, EndedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	screen, _ := render(t, a, map[string]string{
		"acme/open#42":    gh.StateOpen,
		"acme/history#44": gh.StateMerged,
	})
	// The author is a column of its own, so how much space sits between it and
	// the label is whatever the widest row on screen needed.
	for _, want := range []string{
		`open #42 +@open-author`,
		`active #43 +@active-author`,
		`history #44 +@history-author`,
	} {
		if !regexp.MustCompile(want).MatchString(ui.StripANSI(screen)) {
			t.Errorf("dashboard is missing %q:\n%s", want, screen)
		}
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
	ends := map[string]string{}
	for i := range openLimit + 5 {
		key := fmt.Sprintf("acme/web#%d", i+1)
		record(t, a, key, func(r *state.Record) { r.Mark(state.OK, "") })
		ends[key] = gh.StateOpen
	}

	screen, _ := render(t, a, ends)
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

func TestDashboardShowsAutoMergeConfiguration(t *testing.T) {
	a := testApp(t)
	screen, _ := render(t, a, nil)
	if !strings.Contains(ui.StripANSI(screen), "auto-merge off") {
		t.Fatalf("dashboard did not show the safe default:\n%s", screen)
	}

	a.cfg.AutoMergeAgent = true
	a.cfg.AutoMergeBabysit = true
	screen, _ = render(t, a, nil)
	if !strings.Contains(ui.StripANSI(screen), "auto-merge agent+babysit (merge commit)") {
		t.Fatalf("dashboard did not name the enabled sources:\n%s", screen)
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

func TestDashboardShowsDivergenceAnalysisPhase(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	babysit(t, a, loop.Progress{
		PID: 999999, Repo: "acme/api", Number: 42, Title: "tenant scoping",
		StartedAt: now.Add(-90 * time.Minute), MaxIter: 12, MaxCIFixes: 3,
		Round: 12, Phase: loop.PhaseDivergence, Since: now.Add(-2 * time.Minute),
		CI: loop.CIGreen, Reviewed: true, Blockers: 1, Critical: 0, Commits: 5,
	})

	screen, _ := render(t, a, nil)
	if !strings.Contains(screen, "analyzing divergence") {
		t.Fatalf("divergence phase is missing from dashboard:\n%s", screen)
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
		"acme/api#2": gh.StateAutoMerge,
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
	if !strings.Contains(open, "auto-merge queued") {
		t.Errorf("an auto-merge request was not marked: %q", open)
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

// A cache miss is not evidence that a pull request is open. The first frame
// must say that GitHub is being checked without inventing work for the user.
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
	open := sectionOf(t, screen, "OPEN")
	if strings.Contains(open, "api #1") {
		t.Errorf("an unknown pull request was reported as open:\n%s", screen)
	}
	if !strings.Contains(open, "checking GitHub for 1 reviewed PR") {
		t.Errorf("the unknown state was not explained:\n%s", screen)
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
	historySection := ui.StripANSI(sectionOf(t, screen, "HISTORY"))
	// The runs on one pull request share a line, and that line says how many
	// they were: the log keeps them apart, the screen just does not spend a row
	// each on eight reviews of the same branch.
	if got := strings.Count(historySection, "api #42"); got != 1 {
		t.Errorf("two runs on one pull request produced %d lines:\n%s", got, screen)
	}
	// Newest first.
	if strings.Index(historySection, "web #7") > strings.Index(historySection, "api #42") {
		t.Errorf("the history is not newest first:\n%s", screen)
	}
	// The group reports its newest run, which is the fix loop, and the count.
	for _, want := range []string{"2 runs", "nothing found", "reviewer-2 timed out"} {
		if !strings.Contains(historySection, want) {
			t.Errorf("history is missing %q:\n%s", want, historySection)
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
	screen, _ = render(t, a, map[string]string{
		"acme/api#42": gh.StateOpen,
		"acme/web#7":  gh.StateMerged,
	})
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
		r.Author = strings.Repeat("a", 39)
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
		Title: strings.Repeat("ein sehr langer deutscher Titel ", 6), Author: strings.Repeat("b", 39),
		StartedAt: now.Add(-25 * time.Hour), MaxIter: 12, MaxCIFixes: 3,
		Round: 12, Phase: loop.PhaseCIFix, Since: now.Add(-25 * time.Hour),
		CI: loop.CIRed, CIFix: 3, Reviewed: true, Blockers: 12, Critical: 12,
		Commits: 99,
	})

	// A title whose runes and columns disagree, which is what used to push
	// every column to its right out past the edge of the screen.
	record(t, a, "acme/web#4", func(r *state.Record) {
		r.Title = "✨ " + strings.Repeat("日本語のタイトル ", 8)
		r.Author = "example-user"
		r.Mark(state.Pending, "waiting")
	})
	if err := history.Append(a.p.HistoryFile, history.Run{
		Key: "acme/history#5", Author: strings.Repeat("h", 39), Kind: history.KindReview,
		Outcome: history.Failed, Reason: strings.Repeat("failure detail ", 8), EndedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

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

// columnOf is the screen column want starts in, which is what alignment is
// judged by. Byte offsets say nothing about it: the marks and the ellipsis are
// multi byte, and a title in Japanese is half as many runes as columns.
func columnOf(line, want string) int {
	plain := ui.StripANSI(line)
	i := strings.Index(plain, want)
	if i < 0 {
		return -1
	}
	return ui.Cells(plain[:i])
}

// The bug this table replaced: the identity column was sized per line, wide
// when GitHub had told us who opened the pull request and eighteen columns
// narrower when it had not, so a result landed at one of two offsets depending
// on something the result has nothing to do with.
func TestHistoryColumnsDoNotMoveWithTheAuthor(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	for i, author := range []string{"example-user", "", "other-user", ""} {
		if err := history.Append(a.p.HistoryFile, history.Run{
			Key: fmt.Sprintf("acme/api#%d", 40+i), Author: author,
			Kind: history.KindReview, Outcome: history.OK, Reviewed: true,
			EndedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	section := sectionOf(t, mustRender(t, a, 140, nil), "HISTORY")
	var offsets []int
	for _, line := range strings.Split(section, "\n")[1:] {
		offsets = append(offsets, columnOf(line, "nothing found"))
	}
	if len(offsets) != 4 {
		t.Fatalf("expected four history lines, got %d:\n%s", len(offsets), section)
	}
	for _, got := range offsets {
		if got != offsets[0] {
			t.Errorf("the result column moves between lines, offsets %v:\n%s", offsets, section)
			break
		}
	}
}

// Every section draws the same two leading columns at the same width, so a run
// in flight and a run that finished can be read as one list.
func TestSectionsShareTheIdentityColumns(t *testing.T) {
	a := testApp(t)
	record(t, a, "acme/open#42", func(r *state.Record) {
		r.Title = "still open"
		r.Author = "example-user"
		r.Mark(state.OK, "")
	})
	if err := history.Append(a.p.HistoryFile, history.Run{
		Key: "acme/a-much-longer-repository#7", Author: "other-user",
		Kind: history.KindReview, Outcome: history.OK, Reviewed: true, EndedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	screen := mustRender(t, a, 140, map[string]string{"acme/open#42": gh.StateOpen})
	open := columnOf(lineWith(t, screen, "open #42"), "@example-user")
	past := columnOf(lineWith(t, screen, "a-much-longer-repository #7"), "@other-user")
	if open < 0 || past < 0 {
		t.Fatalf("an author column is missing:\n%s", screen)
	}
	// The history line carries a time column the open line does not, so the two
	// start at different offsets; what has to match is the width the label
	// column was given, which is the distance from the label to the author.
	openLabel := columnOf(lineWith(t, screen, "open #42"), "open #42")
	pastLabel := columnOf(lineWith(t, screen, "a-much-longer-repository #7"), "a-much-longer-repository #7")
	if open-openLabel != past-pastLabel {
		t.Errorf("the label column is %d wide in OPEN and %d in HISTORY:\n%s",
			open-openLabel, past-pastLabel, screen)
	}
}

// Repeated runs on one pull request share a line, and that line still says a
// run failed. Losing that is how the state file's one record per pull request
// used to present a branch that failed twice as a clean review.
func TestHistoryGroupsRunsAndKeepsFailuresVisible(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	for i, run := range []history.Run{
		{Outcome: history.Failed, Reason: "reviewer-2 timed out"},
		{Outcome: history.Failed, Reason: "reviewer-1 timed out"},
		{Outcome: history.OK, Reviewed: true},
	} {
		run.Key, run.Kind = "acme/api#42", history.KindReview
		run.EndedAt = now.Add(time.Duration(i) * time.Minute)
		if err := history.Append(a.p.HistoryFile, run); err != nil {
			t.Fatal(err)
		}
	}

	section := ui.StripANSI(sectionOf(t, mustRender(t, a, 140, nil), "HISTORY"))
	if got := strings.Count(section, "api #42"); got != 1 {
		t.Fatalf("three runs on one pull request produced %d lines:\n%s", got, section)
	}
	if !strings.Contains(section, "3 runs, 2 failed") {
		t.Errorf("the group does not say what it collapsed:\n%s", section)
	}
	// The newest run succeeded, so that is what the mark reports.
	if !strings.Contains(section, "nothing found") {
		t.Errorf("the group does not report its newest run:\n%s", section)
	}
}

func mustRender(t *testing.T, a *app, width int, ends map[string]string) string {
	t.Helper()
	var b strings.Builder
	a.dashboard(&ui.Writer{Out: &b, Width: width}, ends)
	return b.String()
}

// A fix loop on a branch has no number and no author, but it does sit in the
// label column, so it has to be measured with everything else. Left out, it
// was cut to a width taken from rows it has nothing to do with, and on a frame
// holding nothing else there was no width to be cut to at all.
func TestBranchRunKeepsItsLabel(t *testing.T) {
	a := testApp(t)
	babysit(t, a, loop.Progress{
		PID: os.Getpid(), Repo: "acme/api", Branch: "feature/crumb-tray",
		StartedAt: time.Now(), MaxIter: 4,
	})

	active := ui.StripANSI(sectionOf(t, mustRender(t, a, 140, nil), "ACTIVE"))
	if !strings.Contains(active, "api feature/crumb-tray") {
		t.Errorf("the branch label was cut away:\n%s", active)
	}
}

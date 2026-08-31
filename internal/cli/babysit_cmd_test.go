package cli

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/ui"
)

func TestManualCommandsDefaultToCurrentBranchPR(t *testing.T) {
	reviewNumber, err := resolveReviewNumber(nil, "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	babysitNumber, context, err := resolveBabysitTarget(nil, "acme/api")
	if err != nil {
		t.Fatal(err)
	}

	if reviewNumber != 0 || babysitNumber != 0 {
		t.Fatalf("default PR numbers = review %d, babysit %d; want both 0 for gh pr view in the current checkout",
			reviewNumber, babysitNumber)
	}
	if len(context) != 0 {
		t.Fatalf("babysit context = %q, want none", context)
	}
}

func TestResolveBabysitTarget(t *testing.T) {
	for _, test := range []struct {
		name        string
		positionals []string
		wantNumber  int
		wantContext []string
		wantErr     string
	}{
		{
			name:        "explicit PR among context",
			positionals: []string{"focus on retries", "42", "keep the API stable"},
			wantNumber:  42,
			wantContext: []string{"focus on retries", "keep the API stable"},
		},
		{
			name:        "context only keeps current branch PR",
			positionals: []string{"focus on retries"},
			wantContext: []string{"focus on retries"},
		},
		{
			name:        "different repository URL",
			positionals: []string{"https://github.com/other/widgets/pull/42"},
			wantErr:     "PR URL is for other/widgets",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			number, context, err := resolveBabysitTarget(test.positionals, "acme/api")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("resolveBabysitTarget(%q) error = %v, want %q",
						test.positionals, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBabysitTarget(%q): %v", test.positionals, err)
			}
			if number != test.wantNumber {
				t.Errorf("resolveBabysitTarget(%q) number = %d, want %d",
					test.positionals, number, test.wantNumber)
			}
			if !reflect.DeepEqual(context, test.wantContext) {
				t.Errorf("resolveBabysitTarget(%q) context = %q, want %q",
					test.positionals, context, test.wantContext)
			}
		})
	}
}

func TestBranchBabysitCreatesAHistoryRun(t *testing.T) {
	run := babysitHistory("acme/api", 0, time.Now(), &loop.Result{
		BranchOnly: true,
		PR: gh.FullPR{
			Title:       "feature/crumb-tray",
			HeadRefName: "feature/crumb-tray",
		},
		Converged: true,
	}, nil)
	if run.Key != "acme/api#branch:feature/crumb-tray" ||
		run.Branch != "feature/crumb-tray" {
		t.Fatalf("history run = %+v", run)
	}
}

func TestConvergedBabysitMergeFailureKeepsConvergedHistory(t *testing.T) {
	mergeErr := errors.New("auto-merge failed: permission denied")
	commentURL := "https://example.invalid/comment/42"
	result := &loop.Result{
		PR: gh.FullPR{
			Number: 42,
			Title:  "fixed change",
		},
		Rounds: 2, Converged: true,
		LastFindings: review.Findings{PR: 42, Questions: 1, CommentURL: &commentURL},
	}
	result.PR.Author.Login = "example-user"
	run := babysitHistory("acme/api", 42, time.Now(), result, mergeErr)

	if run.Outcome != history.Converged || !run.Reviewed || run.Reason != mergeErr.Error() {
		t.Fatalf("history run = %+v", run)
	}
	if run.Rounds != 2 || run.Questions != 1 || run.CommentURL != commentURL {
		t.Fatalf("fix-loop result was not preserved: %+v", run)
	}
}

func TestBabysitDivergenceFlagIsBoolean(t *testing.T) {
	args, err := parseArgs([]string{"42", "--divergence-scan", "focus on retries"}, babysitBoolFlags)
	if err != nil {
		t.Fatal(err)
	}
	if !args.boolean("divergence-scan") {
		t.Fatal("--divergence-scan was not parsed as a boolean")
	}
	if !reflect.DeepEqual(args.pos, []string{"42", "focus on retries"}) {
		t.Fatalf("positionals = %q", args.pos)
	}
}

func TestBabysitHeaderSeparatesReviewAndFixModels(t *testing.T) {
	var out bytes.Buffer
	rep := &loopTermReporter{out: ui.New(os.Stdout).To(&out)}
	// The fix engine is named so its row asserts on a string that appears
	// nowhere else in the header: "engine default" also occurs in the sandbox
	// row, where it says nothing about a model.
	rep.Header(loop.Header{
		Repo: "acme/api", Branch: "feature", Base: "main",
		ReviewEngine: "codex", ReviewModel: "gpt-5.6-terra", ReviewEffort: "medium",
		Engine: "claude", Model: "", Effort: "", MaxIter: 12, FixTimeout: 2 * time.Hour,
	})

	got := out.String()
	for _, want := range []string{
		"review model", "gpt-5.6-terra (effort medium)",
		"fix model", "claude's own choice",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("header is missing %q:\n%s", want, got)
		}
	}
}

// Every step line names its own model. The header states both, but it is gone
// from the screen within the first round of a run that goes on for hours, and
// the label alone does not say which side a step belongs to.
func TestStepLinesNameTheModelThatRanTheStep(t *testing.T) {
	var out bytes.Buffer
	w := ui.New(os.Stdout).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}

	reviewModel := engine.Model{Engine: "codex", Name: "gpt-5.6-terra", Effort: "max"}
	fixModel := engine.Model{Engine: "claude", Name: "opus", Effort: "high"}
	rep.StepEnd("Review round 2 (discarded)", reviewModel, 11*time.Minute, true)
	rep.StepEnd("CI fix 1", fixModel, 11*time.Minute, true)

	got := out.String()
	for _, want := range []string{
		"Review round 2 · gpt-5.6-terra/max · 11m · discarded",
		"CI fix 1 · opus/high · 11m",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("step lines are missing %q:\n%s", want, got)
		}
	}
}

// A model step can run for hours without writing normal output. Its live line
// must exist before the first one-second tick, or the terminal says nothing is
// happening for the whole synchronous fix call.
func TestStepStartImmediatelyShowsWhatIsRunning(t *testing.T) {
	var out bytes.Buffer
	w := (&ui.Writer{Out: os.Stdout, Color: true, Width: 100}).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}
	model := engine.Model{Engine: "codex", Name: "gpt-5.6-terra", Effort: "low"}

	rep.StepStart("Fix round 4", model)

	got := ui.StripANSI(out.String())
	if !strings.Contains(got, "Fix round 4 · gpt-5.6-terra/low · 0s") {
		t.Fatalf("StepStart left the terminal without a live step: %q", got)
	}
}

// Permanent output temporarily borrows the status line. The active step has
// to be redrawn in the same call rather than waiting for a later ticker.
func TestWarningRestoresTheActiveStep(t *testing.T) {
	var out bytes.Buffer
	w := (&ui.Writer{Out: os.Stdout, Color: true, Width: 100}).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}
	model := engine.Model{Engine: "codex", Name: "gpt-5.6-terra", Effort: "low"}

	rep.StepStart("Push fix 1", model)
	out.Reset()
	rep.Warn("push rejected before it reached the remote")

	got := ui.StripANSI(out.String())
	if !strings.Contains(got, "push rejected before it reached the remote\n") ||
		!strings.HasSuffix(got, "Push fix 1 · gpt-5.6-terra/low · 0s") {
		t.Fatalf("warning did not restore the active step immediately: %q", got)
	}
}

func TestActivityImmediatelyNamesNonModelWork(t *testing.T) {
	var out bytes.Buffer
	w := (&ui.Writer{Out: os.Stdout, Color: true, Width: 100}).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}

	rep.Activity("running pre-push verification", 0)

	got := ui.StripANSI(out.String())
	if !strings.Contains(got, "running pre-push verification · 0s") {
		t.Fatalf("Activity left the blocking operation invisible: %q", got)
	}
	rep.ActivityDone()
	if rep.active.shown {
		t.Fatal("finished activity remained live")
	}
}

func TestActivityTicksDoNotFillPipedOutput(t *testing.T) {
	var out bytes.Buffer
	w := ui.New(os.Stdout).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}

	rep.Activity("running pre-push verification", 0)
	rep.Activity("running pre-push verification", time.Second)
	rep.ActivityDone()

	if got, want := out.String(), "running: running pre-push verification\n"; got != want {
		t.Fatalf("plain activity output = %q, want %q", got, want)
	}
}

// The fix model has its own row. It used to hang off "fix sessions", where a
// user looking for a model setting had no reason to open it.
func TestFixModelIsItsOwnSettingRow(t *testing.T) {
	a := testApp(t)
	rows := map[string]setting{}
	for _, s := range a.settings() {
		rows[s.name] = s
	}
	fix, ok := rows["fix model"]
	if !ok {
		t.Fatal("fix model setting is missing")
	}
	cfg := a.cfg
	cfg.FixModel = "gpt-5.6-sol"
	cfg.FixEffort = "high"
	if got := fix.value(cfg); !strings.Contains(got, "gpt-5.6-sol") || !strings.Contains(got, "high") {
		t.Fatalf("fix model value = %q", got)
	}
	if _, ok := rows["fix sessions"]; !ok {
		t.Fatal("fix sessions setting is missing")
	}
}

// An unset value must name who decides. Reviews get a model quorum picks;
// fix sessions get whatever the engine's CLI defaults to, and one label for
// both hid that difference.
func TestUnsetModelRowsNameWhoDecides(t *testing.T) {
	a := testApp(t)
	rows := map[string]setting{}
	for _, s := range a.settings() {
		rows[s.name] = s
	}
	cfg := a.cfg
	cfg.ReviewEngine, cfg.FixEngine = config.EngineClaude, config.EngineClaude
	cfg.ReviewModel, cfg.ReviewEffort = "", ""
	cfg.FixModel, cfg.FixEffort = "", ""

	got := rows["review model"].value(cfg)
	if !strings.Contains(got, review.ClaudeDefaultModel) || !strings.Contains(got, "quorum's default") {
		t.Errorf("review model value = %q", got)
	}
	if got := rows["fix model"].value(cfg); !strings.Contains(got, "claude's own choice") {
		t.Errorf("fix model value = %q", got)
	}
	cfg.ReviewEngine = config.EngineCodex
	if got := rows["review model"].value(cfg); !strings.Contains(got, review.DefaultModel) ||
		!strings.Contains(got, review.DefaultEffort) {
		t.Errorf("codex review model value = %q", got)
	}
}

func TestDefaultEffortOptionHasAVisibleLabel(t *testing.T) {
	opts := defaultEffortOptions(config.EngineCodex, func(c *config.Config) *string { return &c.FixEffort })
	if len(opts) == 0 || opts[0].label != "your codex default" {
		t.Fatalf("effort options = %+v", opts)
	}
}

// The picker may only offer what the selected engine accepts: a level it
// silently ignores looks applied in the config and never reaches the run.
func TestEffortOptionsFollowTheSelectedEngine(t *testing.T) {
	labels := func(eng string) []string {
		var out []string
		for _, o := range defaultEffortOptions(eng, func(c *config.Config) *string { return &c.FixEffort }) {
			out = append(out, o.label)
		}
		return out
	}
	claude := strings.Join(labels(config.EngineClaude), " ")
	if strings.Contains(claude, "ultra") || strings.Contains(claude, "minimal") {
		t.Fatalf("claude was offered a codex-only effort: %s", claude)
	}
	if !strings.Contains(claude, "your claude default") || !strings.Contains(claude, "xhigh") {
		t.Fatalf("claude effort options = %s", claude)
	}
	if !strings.Contains(strings.Join(labels(config.EngineCodex), " "), "ultra") {
		t.Fatal("codex lost its ultra effort")
	}
	grok := strings.Join(labels(config.EngineGrok), " ")
	if strings.Contains(grok, "ultra") || strings.Contains(grok, "xhigh") || strings.Contains(grok, "max") {
		t.Fatalf("grok was offered a non-grok effort: %s", grok)
	}
	if !strings.Contains(grok, "your grok default") || !strings.Contains(grok, "high") {
		t.Fatalf("grok effort options = %s", grok)
	}
}

func TestDivergenceReportReplacesReviewLinkInHistory(t *testing.T) {
	reviewURL := "https://example.invalid/review"
	reportURL := "https://example.invalid/divergence"
	result := &loop.Result{
		PR: gh.FullPR{Number: 42, Title: "retry policy"}, Rounds: 12,
		LastFindings:         review.Findings{PR: 42, Critical: 1, CommentURL: &reviewURL},
		Divergence:           &loop.DivergenceReport{Verdict: loop.DivergenceDiverged},
		DivergenceCommentURL: reportURL,
	}
	run := babysitHistory("acme/api", 42, time.Now(), result, loop.ErrDiverged)
	if run.CommentURL != reportURL || run.Outcome != history.Failed {
		t.Fatalf("history run = %+v", run)
	}
}

func TestDivergenceHasDistinctExitCode(t *testing.T) {
	a := testApp(t)
	if got := a.babysitExit(loop.ErrDiverged); got != exitDiverged {
		t.Fatalf("babysitExit(ErrDiverged) = %d, want %d", got, exitDiverged)
	}
	if exitDiverged == exitNotConverged {
		t.Fatal("diverged and not-converged share an exit code")
	}
}

func TestUnresolvedConflictsHaveADistinctExitCode(t *testing.T) {
	a := testApp(t)
	if got := a.babysitExit(loop.ErrConflicts); got != exitConflicts {
		t.Fatalf("babysitExit(ErrConflicts) = %d, want %d", got, exitConflicts)
	}
	if exitConflicts == exitDiverged || exitConflicts == exitNotConverged {
		t.Fatal("conflicts share an exit code with another failure")
	}
}

func TestBabysitDraftLocalAndConflictFlagsAreBoolean(t *testing.T) {
	args, err := parseArgs([]string{"--draft", "--local", "--no-resolve-conflicts", "42"}, babysitBoolFlags)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"draft", "local", "no-resolve-conflicts"} {
		if !args.boolean(flag) {
			t.Errorf("--%s was not parsed as a boolean", flag)
		}
	}
	if !reflect.DeepEqual(args.pos, []string{"42"}) {
		t.Fatalf("positionals = %q", args.pos)
	}
}

func TestBabysitSummaryReportsTheActualOutcome(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		result   *loop.Result
		mergeErr error
		want     string
		unwanted string
	}{
		{
			name: "round limit",
			err:  errors.Join(loop.ErrNotConverged, errors.New("after 12 review rounds")),
			want: "NOT CONVERGED  after 1 round",
		},
		{
			name:     "dirty worktree",
			err:      errors.New("worktree still has uncommitted changes"),
			want:     "FAILED  worktree still has uncommitted changes",
			unwanted: "NOT CONVERGED",
		},
		{
			name: "divergence",
			err:  loop.ErrDiverged,
			result: &loop.Result{
				PR:         gh.FullPR{Number: 42, HeadRefName: "feature/crumb-tray"},
				Rounds:     12,
				Divergence: &loop.DivergenceReport{Verdict: loop.DivergenceDiverged},
			},
			want:     "DIVERGED  manual decision required",
			unwanted: "FAILED",
		},
		{
			name:     "auto-merge failure",
			err:      errors.New("auto-merge failed: permission denied"),
			mergeErr: errors.New("auto-merge failed: permission denied"),
			want:     "FAILED  auto-merge failed: permission denied",
			unwanted: "NOT CONVERGED",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			rep := &loopTermReporter{out: ui.New(os.Stdout).To(&out)}
			res := test.result
			if res == nil {
				res = &loop.Result{PR: gh.FullPR{Number: 42, HeadRefName: "feature/crumb-tray"}, Rounds: 1}
			}
			rep.summary(res, test.err, "", test.mergeErr)
			got := out.String()
			if !strings.Contains(got, test.want) {
				t.Fatalf("summary is missing %q:\n%s", test.want, got)
			}
			if test.unwanted != "" && strings.Contains(got, test.unwanted) {
				t.Fatalf("summary contains %q:\n%s", test.unwanted, got)
			}
		})
	}
}

// A finished review round used to print twice: once as a step line with model
// and duration, once as a result line with the counts. The step facts are held
// back and merged into the result, so the round is one line.
func TestReviewRoundStepAndResultMergeIntoOneLine(t *testing.T) {
	var out bytes.Buffer
	w := ui.New(os.Stdout).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}
	model := engine.Model{Engine: "codex", Name: "gpt-5.6-terra", Effort: "medium"}

	rep.StepEnd("Review round 1", model, 6*time.Minute, true)
	if out.Len() != 0 {
		t.Fatalf("the review-round step printed before its result:\n%s", out.String())
	}
	rep.RoundResult(1, review.Findings{Critical: 1}, false)

	got := out.String()
	if want := "Review round 1 · gpt-5.6-terra/medium · 6m · 1 critical"; !strings.Contains(got, want) {
		t.Errorf("merged round line is missing %q:\n%s", want, got)
	}
	if strings.Count(got, "Review round 1") != 1 {
		t.Errorf("the round printed more than once:\n%s", got)
	}
}

// A held round line whose RoundResult never arrives (the run failed in
// between) must still reach the terminal with the next output.
func TestPendingReviewRoundLineIsFlushedByLaterOutput(t *testing.T) {
	var out bytes.Buffer
	w := ui.New(os.Stdout).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}
	model := engine.Model{Engine: "codex", Name: "gpt-5.6-terra", Effort: "medium"}

	rep.StepEnd("Review round 3", model, time.Minute, true)
	rep.Info("the review is for another head")

	got := out.String()
	if !strings.Contains(got, "Review round 3 · gpt-5.6-terra/medium · 1m") {
		t.Errorf("the held round line was lost:\n%s", got)
	}
}

// A step that fails used to vanish from the timeline; the summary then named
// an error with no line to point at.
func TestFailedStepPrintsARedLine(t *testing.T) {
	var out bytes.Buffer
	w := ui.New(os.Stdout).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}

	rep.StepEnd("CI fix 2", engine.Model{Engine: "codex", Name: "gpt-5.6-terra", Effort: "low"}, 3*time.Minute, false)

	if got, want := out.String(), "FAIL: CI fix 2 · gpt-5.6-terra/low · 3m · failed"; !strings.Contains(got, want) {
		t.Errorf("failed step line is missing %q:\n%s", want, got)
	}
}

// Warnings sit in the timeline with everything else. Sent to stderr they were
// styled differently from every neighbouring line and, when piped, landed out
// of order.
func TestWarningsGoThroughTheWriter(t *testing.T) {
	var out bytes.Buffer
	w := ui.New(os.Stdout).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}

	rep.Warn("push rejected before it reached the remote")

	if got, want := out.String(), "warn: push rejected before it reached the remote\n"; got != want {
		t.Errorf("Warn wrote %q, want %q", got, want)
	}
}

// The dispute text reaches the terminal once during the run. The summary
// links the posted rebuttal instead of repeating it, and repeats it only when
// posting failed and the terminal is its only copy.
func TestSummaryRepeatsOnlyAnUnpostedRebuttal(t *testing.T) {
	dispute := "DISPUTED FINDINGS:\n1. The fallback is unreachable."
	base := loop.Result{
		PR:     gh.FullPR{Number: 42, HeadRefName: "feature/crumb-tray", URL: "https://github.com/acme/api/pull/42"},
		Rounds: 2, Converged: true, DisputeAccepted: true, DisputeText: dispute,
	}
	render := func(res loop.Result) string {
		var out bytes.Buffer
		w := ui.New(os.Stdout).To(&out)
		rep := &loopTermReporter{out: w, status: w.Status()}
		rep.summary(&res, nil, "", nil)
		return out.String()
	}

	posted := base
	posted.DisputeCommentURL = "https://github.com/acme/api/pull/42#issuecomment-7"
	got := render(posted)
	if !strings.Contains(got, "ok: READY  CI green · review clean after 2 rounds · disputed findings accepted") {
		t.Errorf("verdict line is missing:\n%s", got)
	}
	if !strings.Contains(got, "rebuttal     https://github.com/acme/api/pull/42#issuecomment-7") {
		t.Errorf("rebuttal row is missing:\n%s", got)
	}
	if strings.Contains(got, "The fallback is unreachable") {
		t.Errorf("a posted rebuttal was repeated:\n%s", got)
	}

	got = render(base)
	if !strings.Contains(got, "warn: DISPUTED FINDINGS\n    1. The fallback is unreachable.") {
		t.Errorf("an unposted rebuttal was not shown:\n%s", got)
	}
}

// The commit list is part of the summary grid: the step name and its commits
// line up under the value column like every other row.
func TestSummaryCommitsSitInTheRowGrid(t *testing.T) {
	var out bytes.Buffer
	w := ui.New(os.Stdout).To(&out)
	rep := &loopTermReporter{out: w, status: w.Status()}
	rep.summary(&loop.Result{
		PR: gh.FullPR{Number: 42, HeadRefName: "feature/crumb-tray"}, Rounds: 1, Converged: true,
		RoundLog: []loop.RoundEntry{
			{Label: "CI fix 1", Commits: "1111111 fix: lint\n2222222 fix: format"},
			{Label: "Push fix 1", Commits: "3333333 fix: hook"},
		},
	}, nil, "", nil)

	want := "  commits      CI fix 1    1111111 fix: lint\n" +
		"                           2222222 fix: format\n" +
		"               Push fix 1  3333333 fix: hook\n"
	if got := out.String(); !strings.Contains(got, want) {
		t.Errorf("commit rows are not aligned, want:\n%s\ngot:\n%s", want, got)
	}
}

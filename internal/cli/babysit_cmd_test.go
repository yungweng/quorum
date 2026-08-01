package cli

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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
			want: "not converged",
		},
		{
			name:     "dirty worktree",
			err:      errors.New("worktree still has uncommitted changes"),
			want:     "failed: worktree still has uncommitted changes",
			unwanted: "not converged",
		},
		{
			name: "divergence",
			err:  loop.ErrDiverged,
			result: &loop.Result{
				PR:         gh.FullPR{Number: 42, HeadRefName: "feature/crumb-tray"},
				Rounds:     12,
				Divergence: &loop.DivergenceReport{Verdict: loop.DivergenceDiverged},
			},
			want:     "diverged; manual decision required",
			unwanted: "failed:",
		},
		{
			name:     "auto-merge failure",
			err:      errors.New("auto-merge failed: permission denied"),
			mergeErr: errors.New("auto-merge failed: permission denied"),
			want:     "auto-merge failed: permission denied",
			unwanted: "not converged",
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

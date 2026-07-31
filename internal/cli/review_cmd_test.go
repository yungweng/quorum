package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/review"
)

func TestResolveReviewNumber(t *testing.T) {
	for _, test := range []struct {
		name        string
		positionals []string
		want        int
		wantErr     string
	}{
		{
			name: "no argument selects the current branch PR",
		},
		{
			name:        "bare number",
			positionals: []string{"42"},
			want:        42,
		},
		{
			name:        "matching repository URL",
			positionals: []string{"https://github.com/acme/api/pull/42"},
			want:        42,
		},
		{
			name:        "different repository URL",
			positionals: []string{"https://github.com/other/widgets/pull/42"},
			wantErr:     "PR URL is for other/widgets",
		},
		{
			name:        "invalid argument",
			positionals: []string{"not-a-pr"},
			wantErr:     "expected a PR number",
		},
		{
			name:        "too many arguments",
			positionals: []string{"42", "43"},
			wantErr:     "expected at most one PR argument",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveReviewNumber(test.positionals, "acme/api")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("resolveReviewNumber(%q) error = %v, want %q",
						test.positionals, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveReviewNumber(%q): %v", test.positionals, err)
			}
			if got != test.want {
				t.Fatalf("resolveReviewNumber(%q) = %d, want %d",
					test.positionals, got, test.want)
			}
		})
	}
}

func TestReviewTarget(t *testing.T) {
	for _, test := range []struct {
		name      string
		resolved  int
		requested int
		want      string
	}{
		{
			name: "current branch before GitHub resolves the PR",
			want: "current branch PR",
		},
		{
			name:      "explicit PR before metadata is loaded",
			requested: 42,
			want:      "PR #42",
		},
		{
			name:     "resolved current branch PR",
			resolved: 42,
			want:     "PR #42",
		},
		{
			name:      "resolved PR takes precedence",
			resolved:  43,
			requested: 42,
			want:      "PR #43",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := reviewTarget(test.resolved, test.requested); got != test.want {
				t.Fatalf("reviewTarget(%d, %d) = %q, want %q",
					test.resolved, test.requested, got, test.want)
			}
		})
	}
}

func TestBranchReviewCreatesAHistoryRun(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	rep := &termReporter{
		repo:   "acme/api",
		branch: "feature/crumb-tray",
		title:  "feature/crumb-tray",
	}
	run := rep.historyRun("", started, history.OK, "", nil)
	if run.Key != "acme/api#branch:feature/crumb-tray" ||
		run.Branch != "feature/crumb-tray" {
		t.Fatalf("history run = %+v", run)
	}
}

func TestManualReviewMergeFailureKeepsReviewedHistory(t *testing.T) {
	rep := &termReporter{number: 42, title: "reviewed change", author: "example-user"}
	reason := "auto-merge failed: permission denied"
	run := rep.historyRun("acme/api", time.Now(), history.OK, reason, &review.Result{
		CommentURL: "https://example.invalid/comment/42",
		Findings:   review.Findings{PR: 42, Suggestions: 2},
	})

	if run.Outcome != history.OK || !run.Reviewed || run.Reason != reason {
		t.Fatalf("history run = %+v", run)
	}
	if run.CommentURL == "" || run.Suggestions != 2 {
		t.Fatalf("review result was not preserved: %+v", run)
	}
}

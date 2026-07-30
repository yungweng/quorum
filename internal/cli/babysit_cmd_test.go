package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/loop"
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

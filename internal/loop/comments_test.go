package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/target"
)

func TestFixCommentBodyUsesTheLatestCommentAndLinksTheReview(t *testing.T) {
	current := `Committed the fix.

PR COMMENT:
Die Route wird jetzt vor dem Fallback aufgelöst. Die betroffenen Tests sind grün.`
	original := `PR COMMENT:
This stale text must not be posted.`

	got := fixCommentBody("Review fix round 2", "Review round 2",
		"https://github.com/acme/api/pull/42#issuecomment-7", current, original, "abc123 Fix route")

	for _, want := range []string{
		"### Review fix round 2",
		"[Review round 2](https://github.com/acme/api/pull/42#issuecomment-7)",
		"Die Route wird jetzt vor dem Fallback aufgelöst.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment is missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{MarkerComment, "stale text", "abc123"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("comment contains %q:\n%s", unwanted, got)
		}
	}
}

func TestFixCommentBodyFallsBackToTheStepMessageThenCommits(t *testing.T) {
	original := `Round complete.

PR COMMENT:
Adjusted the retry bound and ran the package tests.`
	got := fixCommentBody("CI fix 1", "", "", "Commit created.", original, "abc123 Adjust retry bound")
	if !strings.Contains(got, "Adjusted the retry bound") || strings.Contains(got, "abc123") {
		t.Fatalf("step-message fallback = %q", got)
	}

	got = fixCommentBody("CI fix 2", "", "", "Commit created.", "", "def456 Fix timeout")
	for _, want := range []string{"### CI fix 2", "Commits:", "def456 Fix timeout"} {
		if !strings.Contains(got, want) {
			t.Errorf("commit fallback is missing %q:\n%s", want, got)
		}
	}
}

func TestPRCommentRejectsProhibitedText(t *testing.T) {
	rep := &warningReporter{}
	r := &run{
		p:   &Pipeline{GH: gh.New("false")},
		o:   Options{RepoRoot: t.TempDir()},
		ctx: context.Background(),
		rep: rep,
		pr:  gh.FullPR{Number: 42},
	}
	body := fixCommentBody("CI fix 1", "", "", "PR COMMENT:\nCodex fixed the check.", "", "abc123 Fix check")
	if _, posted := r.postPRComment("fix-log comment", body); posted {
		t.Fatal("CI fix comment with prohibited text was posted")
	}
	if len(rep.warnings) != 1 || !strings.Contains(rep.warnings[0], "prohibited term") {
		t.Fatalf("warnings = %v", rep.warnings)
	}
}

func TestProhibitedPRCommentTermMatchesWholeWords(t *testing.T) {
	for _, text := range []string{
		"AI updated this.",
		"OpenAI generated this.",
		"codex-generated",
		"one agent",
		"several agents",
		"release_automation",
	} {
		if got := prohibitedPRCommentTerm(text); got == "" {
			t.Errorf("prohibitedPRCommentTerm(%q) found nothing", text)
		}
	}
	for _, text := range []string{"maintain the failure", "agency update", "automatic merge"} {
		if got := prohibitedPRCommentTerm(text); got != "" {
			t.Errorf("prohibitedPRCommentTerm(%q) = %q", text, got)
		}
	}
}

func TestDisputeCommentBodyLinksTheReviewAndDropsTheMarker(t *testing.T) {
	dispute := `DISPUTED FINDINGS:
1. Critical “Home fallback”: /payroll is resolved by PLANNING_SUB_PAGES first.`
	got := disputeCommentBody(1, "https://github.com/acme/api/pull/42#issuecomment-9", dispute)

	for _, want := range []string{
		"### Rebuttal to [review round 1](https://github.com/acme/api/pull/42#issuecomment-9)",
		"1. Critical “Home fallback”",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rebuttal is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, MarkerDisputed) {
		t.Errorf("rebuttal leaked the internal marker:\n%s", got)
	}
	if got := disputeCommentBody(1, "https://example.invalid/review", "No dispute marker."); got != "" {
		t.Errorf("missing marker produced a rebuttal: %q", got)
	}
}

func TestDisputeCommentPostingWarnsWithoutStoppingTheRun(t *testing.T) {
	rep := &warningReporter{}
	r := &run{
		p:   &Pipeline{GH: gh.New("false")},
		o:   Options{RepoRoot: t.TempDir()},
		ctx: context.Background(),
		rep: rep,
		pr:  gh.FullPR{Number: 42},
	}

	url, posted := r.postDisputeComment(1, "https://example.invalid/review",
		"DISPUTED FINDINGS:\n1. The fallback is unreachable.")
	if posted || url != "" {
		t.Fatalf("post result = (%q, %v), want a reported failure", url, posted)
	}
	if len(rep.warnings) != 1 || !strings.Contains(rep.warnings[0], "could not post the rebuttal") {
		t.Fatalf("warnings = %v", rep.warnings)
	}
}

func TestFixCommentDoesNotClaimANoopWasFixed(t *testing.T) {
	rep := &warningReporter{}
	r := &run{
		p:        &Pipeline{GH: gh.New("false"), Git: git.New("false")},
		o:        Options{RepoRoot: t.TempDir()},
		ctx:      context.Background(),
		rep:      rep,
		pr:       gh.FullPR{Number: 42},
		worktree: t.TempDir(),
	}

	r.postFixComment("ci-fix-1", "CI fix 1", "", "", "before-sha")
	if len(rep.warnings) != 0 {
		t.Fatalf("no-op fix tried to post: %v", rep.warnings)
	}
}

func TestDisputeCommentPostsWithoutABacklinkWhenGitHubReturnedNoReviewURL(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' 'https://example.invalid/rebuttal'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := &warningReporter{}
	r := &run{
		p:   &Pipeline{GH: gh.New(bin)},
		o:   Options{RepoRoot: dir},
		ctx: context.Background(),
		rep: rep,
		pr:  gh.FullPR{Number: 42},
	}

	url, posted := r.postDisputeComment(3, "", "DISPUTED FINDINGS:\n1. The lookup already covers this route.")
	if !posted || url != "https://example.invalid/rebuttal" {
		t.Fatalf("post result = (%q, %v)", url, posted)
	}
	if len(rep.warnings) != 1 || !strings.Contains(rep.warnings[0], "without a backlink") {
		t.Fatalf("warnings = %v", rep.warnings)
	}
}

func TestBranchOnlyDisputeDoesNotPost(t *testing.T) {
	rep := &warningReporter{}
	r := &run{
		p:      &Pipeline{GH: gh.New("false")},
		ctx:    context.Background(),
		rep:    rep,
		target: target.Target{BranchOnly: true},
	}
	if _, posted := r.postDisputeComment(1, "", "DISPUTED FINDINGS:\n1. Not real."); posted {
		t.Fatal("branch-only dispute posted a PR comment")
	}
	if len(rep.warnings) != 0 {
		t.Fatalf("branch-only dispute warnings = %v", rep.warnings)
	}
}

type warningReporter struct {
	NopReporter
	warnings []string
}

func (r *warningReporter) Warn(text string) {
	r.warnings = append(r.warnings, text)
}

package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A comment with all five sections and real bullets is the shape everything
// downstream counts on.
const goodComment = `Hi @octocat, thanks for this.

## Summary

The change looks reasonable overall.

## Blockers

- The migration drops the column before the backfill runs.

## Critical

- ` + "`parseAmount`" + ` returns cents where callers expect euros.
- The retry loop has no upper bound.

## Suggestions

- Extract the date handling into its own helper.

## Questions

None.
`

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "final-pr-comment.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateAcceptsAWellFormedComment(t *testing.T) {
	if err := ValidateComment(write(t, goodComment)); err != nil {
		t.Errorf("a well formed comment was rejected: %v", err)
	}
}

func TestCountsBulletsPerSection(t *testing.T) {
	path := write(t, goodComment)
	for _, tc := range []struct {
		heading string
		want    int
	}{
		{"Blockers", 1},
		{"Critical", 2},
		{"Suggestions", 1},
		{"Questions", 0},
	} {
		if got := CountFile(path, tc.heading); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.heading, got, tc.want)
		}
	}
}

func TestVerificationChangesRecordsBothSidesWithoutUnchangedFindings(t *testing.T) {
	final := strings.ReplaceAll(goodComment,
		"- `parseAmount` returns cents where callers expect euros.\n", "")
	final = strings.ReplaceAll(final,
		"- The retry loop has no upper bound.",
		"- The retry loop has no upper bound and can exhaust the request budget.")
	final = strings.ReplaceAll(final, "## Questions\n\nNone.",
		"## Questions\n\n- Should retries share the caller's deadline?")

	changes := filepath.Join(t.TempDir(), "verification-changes.md")
	if err := writeVerificationChanges(write(t, goodComment), write(t, final), changes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(changes)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"Candidate findings removed or rewritten",
		"`parseAmount` returns cents where callers expect euros",
		"The retry loop has no upper bound.",
		"Final findings added or rewritten",
		"can exhaust the request budget",
		"Should retries share the caller's deadline?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("change log is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "migration drops the column") {
		t.Errorf("unchanged finding leaked into the change log:\n%s", got)
	}
}

func TestVerificationChangesSaysWhenReportPassedThrough(t *testing.T) {
	changes := filepath.Join(t.TempDir(), "verification-changes.md")
	if err := writeVerificationChanges(write(t, goodComment), write(t, goodComment), changes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(changes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "No finding changes.") {
		t.Fatalf("unchanged report log = %s", data)
	}
}

// Only Blockers and Critical may keep the fix loop alive. Counting Suggestions
// or Questions as blocking would mean the loop can never converge, because both
// are open-ended by nature.
func TestOnlyBlockersAndCriticalBlock(t *testing.T) {
	f := Findings{Blockers: 1, Critical: 2, Suggestions: 9, Questions: 9}
	if f.Blocking() != 3 {
		t.Errorf("Blocking() = %d, want 3", f.Blocking())
	}
	clean := Findings{Suggestions: 40, Questions: 40}
	if clean.Blocking() != 0 {
		t.Error("suggestions and questions were treated as blocking")
	}
}

// A renamed or missing heading must fail loudly. Silently, it would count as
// zero findings and a PR with real blockers would report itself clean.
func TestValidateRejectsAMissingSection(t *testing.T) {
	broken := `## Summary

Fine.

## Blockers

None.

## Critical

None.

## Suggestions

None.
`
	err := ValidateComment(write(t, broken))
	if err == nil {
		t.Fatal("a comment without ## Questions was accepted")
	}
}

// A finding written as prose instead of a bullet would count as zero.
func TestValidateRejectsProseInsteadOfBullets(t *testing.T) {
	prose := `## Summary

Fine.

## Blockers

There is a serious problem with the migration ordering.

## Critical

None.

## Suggestions

None.

## Questions

None.
`
	if err := ValidateComment(write(t, prose)); err == nil {
		t.Fatal("a prose finding was accepted and would have counted as zero blockers")
	}
}

// Posting the aggregator's narration under the user's own name is worse than
// failing the run.
func TestValidateRejectsMetaOutput(t *testing.T) {
	for _, meta := range []string{
		"I tried to post this but could not.",
		"Run gh pr comment 12 --body-file out.md to post it.",
	} {
		body := "## Summary\n\n" + meta + "\n\n## Blockers\n\nNone.\n\n## Critical\n\nNone.\n\n## Suggestions\n\nNone.\n\n## Questions\n\nNone.\n"
		if err := ValidateComment(write(t, body)); err == nil {
			t.Errorf("meta output was accepted: %q", meta)
		}
	}
}

func TestValidateRejectsACodeFence(t *testing.T) {
	fenced := "```markdown\n" + goodComment + "\n```\n"
	if err := ValidateComment(write(t, fenced)); err == nil {
		t.Fatal("a fenced comment was accepted")
	}
}

// "None." is the documented way to say a section is empty, in the spellings a
// model actually produces.
func TestNoneIsAcceptedInItsUsualSpellings(t *testing.T) {
	for _, none := range []string{"None.", "none", "None", "NONE.", "None!"} {
		body := "## Summary\n\nFine.\n\n## Blockers\n\n" + none +
			"\n\n## Critical\n\nNone.\n\n## Suggestions\n\nNone.\n\n## Questions\n\nNone.\n"
		if err := ValidateComment(write(t, body)); err != nil {
			t.Errorf("%q was rejected: %v", none, err)
		}
	}
}

// Headings are matched case-insensitively with flexible spacing, because that
// is what models produce, but the section still has to be the right one.
func TestSectionMatchingIgnoresCaseAndSpacing(t *testing.T) {
	body := "##   summary\n\nFine.\n\n##  BLOCKERS  \n\n- one\n\n## Critical\n\nNone.\n\n## Suggestions\n\nNone.\n\n## Questions\n\nNone.\n"
	path := write(t, body)
	if err := ValidateComment(path); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if got := CountFile(path, "Blockers"); got != 1 {
		t.Errorf("Blockers = %d, want 1", got)
	}
}

// Grok's multi-turn verifier glues tool narration onto the first heading line
// ("…cascade.## Summary"). Without a cut at the heading, the section parser
// never sees Summary and a paid-for panel is discarded.
func TestNormalizeStripsPreambleAndMidLineGlueBeforeSummary(t *testing.T) {
	body := "I'll independently verify the PR.Reading the core paths for series resolution, roster mirroring, and end cascade.## Summary\n" +
		"@example-user, looks good.\n\n## Blockers\n\nNone.\n\n## Critical\n\nNone.\n\n## Suggestions\n\nNone.\n\n## Questions\n\nNone.\n"
	path := write(t, body)
	if err := ValidateComment(path); err == nil {
		t.Fatal("glued preamble was accepted before normalize; the heading gate would hide the failure mode")
	}
	if err := normalizeAndValidateComment(path); err != nil {
		t.Fatalf("normalized glued output was rejected: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "## Summary\n") {
		t.Fatalf("normalized comment does not start at Summary:\n%s", got)
	}
	if strings.Contains(got, "I'll independently") || strings.Contains(got, "cascade.") {
		t.Fatalf("preamble survived normalize:\n%s", got)
	}
}

func TestNormalizeLeavesACleanCommentUnchanged(t *testing.T) {
	clean := "## Summary\n\nFine.\n\n## Blockers\n\nNone.\n\n## Critical\n\nNone.\n\n## Suggestions\n\nNone.\n\n## Questions\n\nNone.\n"
	if got := normalizeComment(clean); got != clean {
		t.Fatalf("clean comment changed:\n got %q\nwant %q", got, clean)
	}
}

func TestNormalizeLeavesTextWithoutSummaryAlone(t *testing.T) {
	raw := "no headings here at all\n"
	if got := normalizeComment(raw); got != raw {
		t.Fatalf("text without Summary changed: %q", got)
	}
}

// Bullets stop at the next level-2 heading, so a finding is never counted twice
// or attributed to the wrong severity.
func TestBulletsDoNotLeakAcrossSections(t *testing.T) {
	path := write(t, goodComment)
	if got := Count(readFile(t, path), "Summary"); got != 0 {
		t.Errorf("Summary picked up %d bullets from later sections", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

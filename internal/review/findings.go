package review

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Sections are the five headings the aggregated comment must use, in this
// order. The whole machine-readable side of quorum rests on this contract:
// findings.json is produced by counting bullets under these headings, and the
// fix pipeline decides whether to keep looping from those counts.
var Sections = []string{"Summary", "Blockers", "Critical", "Suggestions", "Questions"}

// countedSections are the four that hold findings. Summary is prose.
var countedSections = []string{"Blockers", "Critical", "Suggestions", "Questions"}

// Findings is the machine-readable summary written next to the comment.
type Findings struct {
	Schema             int     `json:"schema"`
	PR                 int     `json:"pr"`
	HeadSHA            string  `json:"head_sha"`
	BaseSHA            string  `json:"base_sha"`
	ReviewersSucceeded int     `json:"reviewers_succeeded"`
	ReviewersRequested int     `json:"reviewers_requested"`
	Blockers           int     `json:"blockers"`
	Critical           int     `json:"critical"`
	Suggestions        int     `json:"suggestions"`
	Questions          int     `json:"questions"`
	CommentFile        string  `json:"comment_file"`
	Posted             bool    `json:"posted"`
	CommentURL         *string `json:"comment_url"`
}

// Blocking is the count that keeps the fix loop alive. Suggestions and
// Questions are handed to a fix round once but must never extend the loop:
// they are open-ended by nature, so treating them as blocking would mean the
// loop can chase moving targets forever and never converge.
func (f Findings) Blocking() int { return f.Blockers + f.Critical }

// Summary renders the counts the way both the status output and the
// notifications phrase them.
func (f Findings) Summary() string {
	return fmt.Sprintf("%d blockers, %d critical, %d suggestions, %d questions",
		f.Blockers, f.Critical, f.Suggestions, f.Questions)
}

var (
	headingRe = regexp.MustCompile(`^##[ \t]+(.*?)[ \t]*$`)
	bulletRe  = regexp.MustCompile(`^[-*][ \t]`)
	fenceRe   = regexp.MustCompile("^```")
	// metaSigns catch an aggregator that narrated what it did instead of
	// producing the comment. Posting one of those to a PR under the user's own
	// name is worse than failing the run.
	metaSigns = regexp.MustCompile(`(?i)I tried to post|gh pr comment|api\.github\.com|exact comment I prepared`)
	// summaryHeadingStart matches the report's opening heading even when a
	// multi-turn engine glues tool narration onto the same line
	// ("…cascade.## Summary"). Case follows the rest of the section matcher.
	summaryHeadingStart = regexp.MustCompile(`(?i)##[ \t]+Summary\b`)
)

// NormalizeCommentFile rewrites path so the postable report starts at its
// first ## Summary heading. Engines that emit tool chatter or a short plan
// before the five sections still yield a machine-readable comment; a file with
// no Summary heading is left alone so ValidateComment can reject it.
func NormalizeCommentFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the comment to normalize: %w", err)
	}
	normalized := normalizeComment(string(data))
	if normalized == string(data) {
		return nil
	}
	if err := os.WriteFile(path, []byte(normalized), 0o644); err != nil {
		return fmt.Errorf("writing the normalized comment: %w", err)
	}
	return nil
}

// normalizeAndValidateComment strips preamble before the five-section report
// and then applies the postable-shape gate.
func normalizeAndValidateComment(path string) error {
	if err := NormalizeCommentFile(path); err != nil {
		return err
	}
	return ValidateComment(path)
}

// normalizeComment drops everything before the first ## Summary heading so
// heading parsers that require a line-start match still see the five sections.
func normalizeComment(text string) string {
	loc := summaryHeadingStart.FindStringIndex(text)
	if loc == nil {
		return text
	}
	out := strings.TrimSpace(text[loc[0]:])
	if out == "" {
		return text
	}
	return out + "\n"
}

// ValidateComment checks that the aggregator produced something postable.
//
// Every rule here exists because findings.json is only trustworthy when the
// structure is exactly as specified: a renamed heading or a finding written as
// prose instead of a bullet would silently count as zero findings, and a run
// with real blockers would report itself clean.
func ValidateComment(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the aggregated comment: %w", err)
	}
	text := string(data)

	lines := strings.Split(text, "\n")
	if len(lines) > 0 && fenceRe.MatchString(lines[0]) {
		return fmt.Errorf("aggregator wrapped the comment in a code fence")
	}
	if metaSigns.MatchString(text) {
		return fmt.Errorf("aggregator produced meta-output instead of a PR comment")
	}

	present := headings(text)
	for _, want := range Sections {
		if !present[strings.ToLower(want)] {
			return fmt.Errorf("aggregator output is missing the '## %s' section", want)
		}
	}
	for _, want := range countedSections {
		if !sectionContentOK(text, want) {
			return fmt.Errorf("aggregator section '## %s' has neither bullet findings nor 'None.'", want)
		}
	}
	return nil
}

// Count returns the number of top-level bullets under a heading.
func Count(text, heading string) int {
	n := 0
	forEachLineIn(text, heading, func(line string) {
		if bulletRe.MatchString(line) {
			n++
		}
	})
	return n
}

// CountFile counts a heading's bullets in a file on disk.
func CountFile(path, heading string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return Count(string(data), heading)
}

// writeVerificationChanges records every finding block that differs between
// the aggregated candidate and final report. Exact matches disappear from the
// log. A rewrite therefore leaves both its old form under "Candidate" and its
// new form under "Final", without adding a second PR comment.
func writeVerificationChanges(candidatePath, verifiedPath, changesPath string) error {
	candidate, err := os.ReadFile(candidatePath)
	if err != nil {
		return fmt.Errorf("reading the aggregated candidate: %w", err)
	}
	verified, err := os.ReadFile(verifiedPath)
	if err != nil {
		return fmt.Errorf("reading the verified comment: %w", err)
	}

	var b strings.Builder
	b.WriteString("# Verification changes\n\n")
	b.WriteString("Local audit only. The final PR comment is the verified report.\n")
	changed := false
	for _, section := range countedSections {
		before, _ := sectionFindings(string(candidate), section)
		after, _ := sectionFindings(string(verified), section)
		removed, added := findingDifference(before, after)
		if len(removed) == 0 && len(added) == 0 {
			continue
		}
		changed = true
		fmt.Fprintf(&b, "\n## %s\n", section)
		writeChangedFindings(&b, "Candidate findings removed or rewritten", removed)
		writeChangedFindings(&b, "Final findings added or rewritten", added)
	}
	if !changed {
		b.WriteString("\nNo finding changes.\n")
	}
	if err := os.WriteFile(changesPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing verification changes: %w", err)
	}
	return nil
}

func findingDifference(before, after []string) (removed, added []string) {
	afterCount := make(map[string]int, len(after))
	for _, finding := range after {
		afterCount[finding]++
	}
	for _, finding := range before {
		if afterCount[finding] > 0 {
			afterCount[finding]--
			continue
		}
		removed = append(removed, finding)
	}

	beforeCount := make(map[string]int, len(before))
	for _, finding := range before {
		beforeCount[finding]++
	}
	for _, finding := range after {
		if beforeCount[finding] > 0 {
			beforeCount[finding]--
			continue
		}
		added = append(added, finding)
	}
	return removed, added
}

func writeChangedFindings(b *strings.Builder, title string, findings []string) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### %s\n\n", title)
	for _, finding := range findings {
		for _, line := range strings.Split(finding, "\n") {
			fmt.Fprintf(b, "> %s\n", line)
		}
		b.WriteByte('\n')
	}
}

// sectionFindings returns complete top-level finding blocks. A block starts at
// a bullet and includes its continuation lines through the next bullet, so the
// changelog compares the complete old and new wording. stray is non-empty prose
// before the first bullet; "None." is allowed for an empty section.
func sectionFindings(text, heading string) (out []string, stray string) {
	var current []string
	flush := func() {
		for len(current) > 1 && strings.TrimSpace(current[len(current)-1]) == "" {
			current = current[:len(current)-1]
		}
		if len(current) > 0 {
			out = append(out, strings.Join(current, "\n"))
		}
		current = nil
	}

	forEachLineIn(text, heading, func(line string) {
		if bulletRe.MatchString(line) {
			flush()
			current = []string{line}
			return
		}
		if current != nil {
			current = append(current, line)
			return
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" && !isNone(trimmed) && stray == "" {
			stray = trimmed
		}
	})
	flush()
	return out, stray
}

// sectionContentOK accepts a section that either holds bullets or says "None."
// and nothing else. A section with prose but no bullets would count as zero
// findings while actually describing some, so it fails instead.
func sectionContentOK(text, heading string) bool {
	bullets, textLines, other := 0, 0, 0
	forEachLineIn(text, heading, func(line string) {
		switch {
		case bulletRe.MatchString(line):
			bullets++
		case strings.TrimSpace(line) != "":
			textLines++
			if !isNone(line) {
				other++
			}
		}
	})
	return bullets > 0 || (textLines > 0 && other == 0)
}

var noneRe = regexp.MustCompile(`(?i)^none[.!]?([ \t]|$)`)

func isNone(line string) bool { return noneRe.MatchString(strings.TrimSpace(line)) }

// forEachLineIn calls fn for every line under the given level-2 heading, up to
// the next level-2 heading.
func forEachLineIn(text, heading string, fn func(string)) {
	want := strings.ToLower(heading)
	inside := false
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := headingRe.FindStringSubmatch(line); m != nil {
			inside = strings.EqualFold(strings.TrimSpace(m[1]), want)
			continue
		}
		if inside {
			fn(line)
		}
	}
}

func headings(text string) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if m := headingRe.FindStringSubmatch(sc.Text()); m != nil {
			out[strings.ToLower(strings.TrimSpace(m[1]))] = true
		}
	}
	return out
}

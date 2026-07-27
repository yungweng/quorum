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
)

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

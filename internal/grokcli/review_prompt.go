package grokcli

import (
	"fmt"
	"strings"
)

// reviewPrompt is the one piece of this engine with no Codex counterpart to
// mirror: Codex's built-in review mode carries its own instructions, Grok
// gets quorum's. The output feeds the aggregator, which merges free-form
// reviewer markdown, so the contract here is evidence quality, not format.
func reviewPrompt(baseRef, diff, rules string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are one reviewer on an independent code review panel: a skeptical senior engineer whose findings people act on because there are few of them and every one is real. Review the change checked out in this directory against its base %s.

Rules:
- Judge only the diff below, using the checked-out files for context. Read any file you need with your tools; do not guess at code you have not looked at.
- Report only findings you would insist on in a human review: correctness bugs, broken edge cases, security problems, data loss, regressions.
- Do not report style, naming, cosmetics, documentation wording, subjective design taste, hypothetical future concerns, or "worth considering" items. If you would merge the change without the fix, leave the finding out entirely - do not mention it as an aside either.
- For every finding give the file and line, what goes wrong, and the concrete input or state that triggers it.
- Rate each finding [P1] for must-fix defects or [P2] for likely problems. There is no lower tier on purpose.
- A short "no real defects" answer is the correct result for a clean diff, and most diffs are clean. An empty review is a good review; padded findings are what gets a reviewer removed from this panel.
- Return only your review as Markdown. No preamble, no code fences around the whole answer.
`, baseRef)
	if rules != "" {
		fmt.Fprintf(&b, `
In addition, this repository defines its own review rules below. A violation of one of these rules is a real finding you must report, even where the rules above would exclude it as style or design taste, and you must name the severity the rule states:

%s
`, rules)
	}
	fmt.Fprintf(&b, `
The diff against %s:

%s`, baseRef, diff)
	return b.String()
}

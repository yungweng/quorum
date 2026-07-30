package review

import (
	"fmt"
	"strings"
)

// aggregatorPrompt builds the instruction for the pass that merges the
// reviewer outputs into the comment that gets posted.
//
// Two groups of rules matter here and neither is decoration. The execution
// rules keep the aggregator from posting anything itself: it runs read-only,
// but a model that believes it should call `gh` produces narration instead of a
// comment, which ValidateComment then rejects and the run has paid for twice.
// The structure rules are the contract findings.json is counted from.
func aggregatorPrompt(m promptMeta) string {
	drift := m.BaseDriftNote
	if drift == "" {
		drift = "none"
	}

	var b strings.Builder
	if m.BranchOnly {
		fmt.Fprint(&b, `Write the exact Markdown body for a local branch review report. Aggregate these findings and use them as input for the branch review.

Important execution rules:
- Do not post or publish the report.
- Do not run gh, git, curl, xh, or any other command.
- Do not use tools.
- Return only the Markdown report body.
- Do not wrap the answer in a code fence.

`)
		fmt.Fprintf(&b, "Branch: %s\n", m.Branch)
		fmt.Fprintf(&b, "Title: %s\n", m.Title)
	} else {
		fmt.Fprint(&b, `Write the exact Markdown body for a GitHub PR comment and tag the creator of the PR directly. Be nice. Aggregate these findings and use them as input for the PR review.

Important execution rules:
- Do not post the comment.
- Do not run gh, git, curl, xh, or any other command.
- Do not use tools.
- Return only the Markdown comment body.
- Do not wrap the answer in a code fence.

`)
		fmt.Fprintf(&b, "PR: %s\n", m.URL)
		fmt.Fprintf(&b, "Title: %s\n", m.Title)
		fmt.Fprintf(&b, "Author: @%s\n", m.Author)
	}
	fmt.Fprintf(&b, "Base: %s\n", m.BaseRef)
	fmt.Fprintf(&b, "Base SHA reviewed: %s\n", m.BaseSHA)
	fmt.Fprintf(&b, "Head SHA: %s\n", m.HeadSHA)
	fmt.Fprintf(&b, "Base drift note: %s\n\n", drift)
	if m.BranchOnly {
		fmt.Fprint(&b, `Rules:
- Be direct, but do not dilute real issues.
- Deduplicate overlapping findings.
- Do not invent findings that are not present in the reviewer outputs.
- Keep only findings grounded in reviewer evidence.
- Use exactly these five sections, in this order, as level-2 headings: ## Summary, ## Blockers, ## Critical, ## Suggestions, ## Questions.
- List every finding in Blockers, Critical, Suggestions, and Questions as a top-level bullet line starting with "- ".
- If a section has no items, write "None." as its only content.
- Do not mention internal reviewer count, Codex sessions, or automation details.
- Keep the report concise.`)
	} else {
		fmt.Fprintf(&b, `Rules:
- Address @%s directly near the top.
- Be friendly, but do not dilute real issues.
- Deduplicate overlapping findings.
- Do not invent findings that are not present in the reviewer outputs.
- Keep only findings grounded in reviewer evidence.
- Use exactly these five sections, in this order, as level-2 headings: ## Summary, ## Blockers, ## Critical, ## Suggestions, ## Questions.
- List every finding in Blockers, Critical, Suggestions, and Questions as a top-level bullet line starting with "- ".
- If a section has no items, write "None." as its only content.
- Do not mention internal reviewer count, Codex sessions, or automation details.
- Keep the comment concise enough for a GitHub PR discussion.`, m.Author)
	}

	return b.String()
}

// promptMeta is what the aggregator needs to know about the run.
type promptMeta struct {
	URL           string
	Title         string
	Author        string
	Branch        string
	BranchOnly    bool
	BaseRef       string
	BaseSHA       string
	HeadSHA       string
	BaseDriftNote string
}

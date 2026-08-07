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
- The very first line of your answer must be exactly "## Summary", including both leading # characters. Every section heading keeps its leading "## ". These headings are parsed by a machine; a heading without them counts as missing.
- List every finding in Blockers, Critical, Suggestions, and Questions as a top-level bullet line starting with "- ".
- Suggestions are for changes the author should actually make. Drop polish, taste, naming, and hypothetical improvements even when a reviewer mentioned them; "None." is the right content for a clean change, not a weak bullet list.
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
- The very first line of your answer must be exactly "## Summary", including both leading # characters. Every section heading keeps its leading "## ". These headings are parsed by a machine; a heading without them counts as missing.
- List every finding in Blockers, Critical, Suggestions, and Questions as a top-level bullet line starting with "- ".
- Suggestions are for changes the author should actually make. Drop polish, taste, naming, and hypothetical improvements even when a reviewer mentioned them; "None." is the right content for a clean change, not a weak bullet list.
- If a section has no items, write "None." as its only content.
- Do not mention internal reviewer count, Codex sessions, or automation details.
- Keep the comment concise enough for a GitHub PR discussion.`, m.Author)
	}

	return b.String()
}

// verifierPrompt asks a fresh pass to judge the aggregated findings against
// the checked-out code. It sees the candidate report, not the raw reviewer
// discussion, so repetition cannot replace evidence. The prompt is neutral:
// true findings survive, false findings go, and useful corrections stay
// visible through the changelog Go derives from both reports.
func verifierPrompt(m promptMeta) string {
	var target strings.Builder
	if m.BranchOnly {
		fmt.Fprintf(&target, "Branch: %s\nTitle: %s\n", m.Branch, m.Title)
	} else {
		fmt.Fprintf(&target, "PR: %s\nTitle: %s\nAuthor: @%s\n", m.URL, m.Title, m.Author)
	}
	fmt.Fprintf(&target, "Base: %s\nBase SHA reviewed: %s\nHead SHA: %s", m.BaseRef, m.BaseSHA, m.HeadSHA)

	return fmt.Sprintf(`Act as an independent evidence verifier for the candidate code-review report on stdin.

%s

The candidate report contains claims that require checking. Some may be true positives, some may be false positives, and some may identify a real issue imprecisely. Do not aim for any predetermined outcome or proportion. Judge each finding on the repository evidence, independent of reviewer agreement or wording.

Inspect the local diff and relevant code for every bullet under Blockers, Critical, Suggestions, and Questions. Trace the actual control and error paths. Use read-only inspection first. You may run focused commands or tests when they materially help verify a claim. Do not edit source, tests, configuration, generated files, or repository metadata. Do not stage or commit anything. Put any disposable artifact you intentionally create under $TMPDIR or /tmp, outside the repository.

For each candidate finding:
- Preserve it unchanged when it is accurate, sufficiently specific, and correctly rated.
- Rewrite it when the underlying issue is real but its scope, severity, conditions, location, explanation, or evidence needs correction. Keep the final wording concise and concrete.
- Remove the entire finding when it is wrong, already handled, intended behavior, outside the changed code's responsibility, purely speculative, or unsupported after reasonable investigation.
- Add a new finding only when verification directly reveals a distinct, high-confidence defect within the changed code or its immediate behavior. Do not add unrelated cleanup, style preferences, or speculative hardening.

Do not favor keeping, changing, adding, or removing findings. Apply the same evidence standard to every outcome.

Output rules:
- Return only the complete final Markdown report body. Do not wrap it in a code fence.
- Use exactly these five sections, in this order, as level-2 headings: ## Summary, ## Blockers, ## Critical, ## Suggestions, ## Questions.
- The very first line of your answer must be exactly "## Summary", including both leading # characters. Every section heading keeps its leading "## ". These headings are parsed by a machine; a heading without them counts as missing.
- Do not write any preamble, plan, status update, or tool narration before, between, or after the five sections. Tool use and reasoning stay off the answer; the answer is only the report body.
- List every final finding as a top-level bullet beginning with "- ". If a section has no findings, write "None." as its only content.
- Rewrite the summary so it describes only the final findings. Do not discuss discarded claims or the verification process.
- Preserve a concise opening suitable for the candidate's target. Do not mention reviewers, verification, Codex, or automation.
- Do not use the network, post comments, or run gh, curl, or xh.`, target.String())
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

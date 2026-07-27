package loop

import (
	"fmt"
	"strings"
)

// Markers the fix session uses to signal state back to the pipeline. They are
// matched at the start of a line in the session's final message.
const (
	// MarkerQuestions means the session stopped for a product decision.
	MarkerQuestions = "OPEN QUESTIONS:"
	// MarkerDisputed means the session believes the remaining Blocker/Critical
	// findings are wrong and no code change is warranted.
	MarkerDisputed = "DISPUTED FINDINGS:"
	// MarkerComment introduces the fix-log text the pipeline posts to the PR.
	MarkerComment = "PR COMMENT:"
)

// standingRules are sent once, with the first prompt, and stay in effect for
// every later step because all of them resume the same session.
//
// Several lines here are load-bearing rather than stylistic:
//   - the exact push command, because a bare `git push` fails on the detached
//     checkout the pipeline works in;
//   - the ban on creating comments, because the pipeline posts the fix log
//     itself via gh so it appears as a normal comment from the user;
//   - the DISPUTED FINDINGS contract, which is the only way a round with no
//     commits is allowed to end without stopping the run.
func standingRules(branch string, autonomous bool) string {
	decision := `- Only for product decisions you cannot responsibly make yourself (data semantics, user-visible policy, migration of existing data): stop and end your final message with a line that is exactly:
OPEN QUESTIONS:
followed by short numbered questions. Use this marker for nothing else. You will receive the user's answers and can then continue.`
	if autonomous {
		decision = `- No human is available during this run. Make every decision yourself, including product decisions: pick the most conservative reasonable option and record notable decisions in the PR comment section of your final message. Do not stop to ask questions.`
	}

	return fmt.Sprintf(`You are the fix agent for an existing pull request, running unattended inside an automated pipeline (CI checks -> external code review -> fix rounds), in a dedicated git worktree on a detached checkout of the PR head.

Standing rules for every step:
- Follow this repository's contributor and agent instructions (AGENTS.md, CLAUDE.md, CONTRIBUTING) where present.
- Keep changes minimal and focused on the findings you are asked to address; match the existing code style and do not refactor beyond that.
- Write clean, descriptive commit messages. Never mention AI, agents, Codex, or automation anywhere.
- You are on a detached checkout, so push with exactly: git push origin HEAD:refs/heads/%s
- Only push when a step explicitly tells you to. Never create or edit PRs or comments, never rewrite published history.
- Make reasonable technical decisions yourself.
%s
- If an external review finding rated Blocker or Critical is factually wrong and no code change is warranted, do not change code just to satisfy the review. Instead end your final message with a line that is exactly:
DISPUTED FINDINGS:
followed by short numbered rebuttals, one per disputed finding, each naming the finding and why it is wrong. Use this marker only for Blocker/Critical findings you are confident are incorrect, never to avoid work, and never together with code changes that already resolve the finding.`,
		branch, decision)
}

// firstPrompt wraps the opening step in the standing rules and PR context.
func firstPrompt(rules, prContext, step string) string {
	return fmt.Sprintf("%s\n\n## Pull request\n\n%s\n\n## Current step\n\n%s", rules, prContext, step)
}

// ciFixPrompt asks the session to fix red checks.
//
// The closing sentence matters for cost: an earlier version had Codex watch CI
// itself, which was pure idle time billed to the fix session while the pipeline
// was already watching the same checks.
func ciFixPrompt(number int, failsJSON string) string {
	return fmt.Sprintf(`CI on PR #%d is red. Failing checks as JSON:

%s

Inspect the failures with gh (for example: gh run view <run-id> --log-failed; the run id is the number after /runs/ in the link). Reproduce locally where practical, fix the problems, run the affected checks locally, commit, and push. Do not wait for CI; the pipeline watches the checks for you and starts another fix session if they are still red.`,
		number, failsJSON)
}

// fixRoundPrompt hands a review round's findings to the session.
//
// Suggestions and Questions are handed over once per round but never keep the
// loop alive; only Blockers and Critical do. The PR COMMENT block is what the
// pipeline posts afterwards.
func fixRoundPrompt(number int, comment string) string {
	return fmt.Sprintf(`An external code review of PR #%d found issues. The full review comment follows below.

Please check for each finding whether it is a real issue or might be intended behavior.
- For every real issue that is not intended: plan and fix it. This is mandatory for Blockers and Critical findings.
- Suggestions: implement each one unless it describes intended behavior or you have a strong technical reason not to.
- Resolve the Questions with your best judgment; only raise OPEN QUESTIONS if one truly needs a product decision.
- Run the affected checks, commit your work, and push. Do not wait for CI; the pipeline watches the checks for you.

Unless you end with the DISPUTED FINDINGS marker, end your final message with a line that is exactly:
%s
followed by a short log comment for the PR: what you fixed and how, and which findings you left unchanged because they are intended. Write it in the language of the PR description, plain and factual, and never mention AI, agents, or automation in it. Use this marker for nothing else.

---

%s`, number, MarkerComment, comment)
}

// commitPrompt asks the session to commit what it left in the working tree.
const commitPrompt = `There are uncommitted changes in the worktree. Review them and commit everything that belongs to the task (git add + git commit with a descriptive message). Do not push.`

// bounceQuestionsPrompt sends open questions back in autonomous mode.
const bounceQuestionsPrompt = `No human is available during this automated run. Decide these questions yourself: pick the most conservative reasonable option for each, apply it, and record the decision in the PR comment section of your final message. Continue with the current step. All rules from the initial instructions still apply.`

// forceRecheckPrompt is the adversarial pass a dispute has to survive before it
// is accepted. Without it, the session could end any round it disliked by
// declaring the findings wrong, and the run would finish on its own say-so.
const forceRecheckPrompt = `Re-examine your disputed findings adversarially, as if a colleague insisted they are real. For each one, actively try to construct the failure scenario the reviewer describes and check the code against it. If a finding turns out to be real after all, implement the fix and commit. Only if you remain confident it is wrong after this re-check, explain why and end your message with the DISPUTED FINDINGS: marker again. All rules from the initial instructions still apply.`

// answersPrompt feeds a human's answers back into the session.
func answersPrompt(answers string) string {
	return fmt.Sprintf(`Answers from the user to your open questions:

%s
Continue with the current step using these decisions. All rules from the initial instructions still apply.`, answers)
}

// disputeReplyPrompt feeds a human's pushback on a dispute back into the session.
func disputeReplyPrompt(reply string) string {
	return fmt.Sprintf(`The user does not accept your dispute as-is and replies:

%s
Reconsider the disputed findings with this input. If the user is right, implement the fix and commit. If you still believe a finding is wrong, explain why and end your message with the DISPUTED FINDINGS: marker again. All rules from the initial instructions still apply.`, reply)
}

// prContext describes the pull request to the session.
func prContext(number int, title, branch, base, url, body, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PR #%d: %s\nBranch: %s (base: %s)\nURL: %s\n\nPR description:\n\n%s",
		number, title, branch, base, url, body)
	if extra != "" {
		fmt.Fprintf(&b, "\n\nAdditional context from the user:\n\n%s", extra)
	}
	return b.String()
}

// section returns everything from the line that starts with marker to the end
// of the message, or "" when the marker is absent.
func section(text, marker string) string {
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, marker) {
			return strings.Join(strings.Split(text, "\n")[i:], "\n")
		}
	}
	return ""
}

// hasMarker reports whether a marker appears at the start of any line.
func hasMarker(text, marker string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

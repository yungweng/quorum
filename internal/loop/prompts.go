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
func standingRules(branch string, autonomous, branchOnly, offline bool, repoRules string) string {
	target := "an existing pull request"
	pipeline := "CI checks -> external code review -> fix rounds"
	record := "in the PR comment section of your final message"
	if branchOnly {
		target = "a pushed branch that has no open pull request"
		pipeline = "external code review -> fix rounds"
		record = "in your final message"
	}
	if offline {
		pipeline = "external code review -> fix rounds -> local tests, with one push and one CI run at the very end"
	}
	decision := `- Only for product decisions you cannot responsibly make yourself (data semantics, user-visible policy, migration of existing data): stop and end your final message with a line that is exactly:
OPEN QUESTIONS:
followed by short numbered questions. Use this marker for nothing else. You will receive the user's answers and can then continue.`
	if autonomous {
		decision = fmt.Sprintf("- No human is available during this run. Make every decision yourself, including product decisions: pick the most conservative reasonable option and record notable decisions %s. Do not stop to ask questions.", record)
	}

	pushRules := fmt.Sprintf(`- You are on a detached checkout, so push with exactly: git push origin HEAD:refs/heads/%s
- Only push when a step explicitly tells you to. Never create or edit PRs or comments, never rewrite published history.`, branch)
	if offline {
		// CI repair steps after the run's single final push are the one place
		// a step still tells the session to push.
		pushRules = fmt.Sprintf(`- Do not push. The pipeline pushes the branch itself, once, at the end of the run. Only when a step explicitly tells you to push, push with exactly: git push origin HEAD:refs/heads/%s
- Never create or edit PRs or comments, never rewrite published history.`, branch)
	}
	rules := fmt.Sprintf(`You are the fix agent for %s, running unattended inside a pipeline (%s), in a dedicated git worktree on a detached checkout of the pushed branch head.

Standing rules for every step:
- Follow this repository's contributor and agent instructions (AGENTS.md, CLAUDE.md, CONTRIBUTING) where present.
- Keep changes minimal and focused on the findings you are asked to address; match the existing code style and do not refactor beyond that.
- Write clean, descriptive commit messages. Never mention AI, agents, Codex, or automation anywhere.
%s
- Put screenshots and other disposable QA artifacts in $TMPDIR or /tmp, not in the worktree. Before finishing a step, leave git status --porcelain empty: commit task files and remove only temporary artifacts you created.
- Make reasonable technical decisions yourself.
%s
- If an external review finding rated Blocker or Critical is factually wrong and no code change is warranted, do not change code just to satisfy the review. Instead end your final message with a line that is exactly:
DISPUTED FINDINGS:
followed by short numbered rebuttals, one per disputed finding, each naming the finding and why it is wrong. Use this marker only for Blocker/Critical findings you are confident are incorrect, never to avoid work, and never together with code changes that already resolve the finding.`,
		target, pipeline, pushRules, decision)

	if repoRules != "" {
		rules += fmt.Sprintf(`

This repository additionally defines its own rules below. The external review enforces them, so apply them to every change you make; never resolve a finding in a way that itself violates one of them:

%s`, repoRules)
	}
	return rules
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

Inspect the failures with gh (for example: gh run view <run-id> --log-failed; the run id is the number after /runs/ in the link). Reproduce locally where practical, fix the problems, run the affected checks locally, commit, and push. Do not wait for CI; the pipeline watches the checks for you and starts another fix session if they are still red.

End your final message with a line that is exactly:
%s
followed by a short log comment for the PR: what failed, what you changed and how, and which checks you ran. Write it in the language of the PR description, plain and factual, and never mention AI, agents, automation, or the name of any coding tool in it. Use this marker for nothing else.`,
		number, failsJSON, MarkerComment)
}

// testFixPrompt asks the session to repair the repository's local test
// command, which the pipeline itself ran in the worktree. It is the offline
// counterpart of ciFixPrompt: same repair loop, but the evidence is the
// command's own output instead of GitHub's failing checks.
func testFixPrompt(testCmd, logTail string) string {
	return fmt.Sprintf(`The pipeline ran the repository's test command in the worktree and it failed.

Command: %s

Last lines of its output:

%s

Fix the problems, run the command yourself to confirm it passes, and commit. Do not push; the pipeline reruns the command for you.

End your final message with a line that is exactly:
%s
followed by a short log comment for the PR: what failed, what you changed and how. Write it plain and factual, and never mention AI, agents, automation, or the name of any coding tool in it. Use this marker for nothing else.`,
		testCmd, logTail, MarkerComment)
}

// pushFixPrompt asks the session to repair whatever the repository's own
// pre-push verification refused. It is the third repair prompt beside
// ciFixPrompt and testFixPrompt, and the strictest: the shortest way out of a
// rejected push is to disable the check, so the prompt rules that out.
func pushFixPrompt(branch, logTail string) string {
	return fmt.Sprintf(`The pipeline tried to push %s and the push was refused before it reached the remote. The repository verifies commits locally on push, and that verification failed.

Its output:

%s

Fix what it reports, in the code. Then run the same check yourself to confirm it passes, and commit.

Hard rules for this step:
- Do not push. The pipeline pushes for you and repeats the verification.
- Do not bypass the verification: no --no-verify, no --force, no changes to the hook configuration, and no loosening of a lint, type or dead-code rule to make the complaint disappear. A run that does this is stopped.
- If the complaint is about unused or unreachable code you added, remove it or wire it up; do not silence the rule.

End your final message with a line that is exactly:
%s
followed by a short log comment for the PR: what the check reported, what you changed and how. Write it plain and factual, and never mention AI, agents, automation, or the name of any coding tool in it. Use this marker for nothing else.`,
		branch, logTail, MarkerComment)
}

// fixRoundPrompt hands a review round's findings to the session.
//
// Suggestions and Questions are handed over once per round but never keep the
// loop alive; only Blockers and Critical do. The PR COMMENT block is what the
// pipeline posts afterwards.
func fixRoundPrompt(number int, branch string, branchOnly, offline bool, comment string) string {
	finish := "Run the affected checks, commit your work, and push. Do not wait for CI; the pipeline watches the checks for you."
	finishBranch := "Run the affected checks, commit your work, and push."
	if offline {
		finish = "Run the affected checks and commit your work. Do not push."
		finishBranch = finish
	}
	if branchOnly {
		return fmt.Sprintf(`An external code review of branch %s found issues. The full review report follows below.

Please check for each finding whether it is a real issue or might be intended behavior.
- For every real issue that is not intended: plan and fix it. This is mandatory for Blockers and Critical findings.
- Suggestions: implement each one unless it describes intended behavior or you have a strong technical reason not to.
- Resolve the Questions with your best judgment; only raise OPEN QUESTIONS if one truly needs a product decision.
- %s

---

%s`, branch, finishBranch, comment)
	}
	return fmt.Sprintf(`An external code review of PR #%d found issues. The full review comment follows below.

Please check for each finding whether it is a real issue or might be intended behavior.
- For every real issue that is not intended: plan and fix it. This is mandatory for Blockers and Critical findings.
- Suggestions: implement each one unless it describes intended behavior or you have a strong technical reason not to.
- Resolve the Questions with your best judgment; only raise OPEN QUESTIONS if one truly needs a product decision.
- %s

Unless you end with the DISPUTED FINDINGS marker, end your final message with a line that is exactly:
%s
followed by a short log comment for the PR: what you fixed and how, which checks you ran, and which findings you left unchanged because they are intended. Write it in the language of the PR description, plain and factual, and never mention AI, agents, or automation in it. Use this marker for nothing else.

---

%s`, number, finish, MarkerComment, comment)
}

// suggestionRoundPrompt hands the Suggestions of the final, otherwise clean
// review to the session, once. No review round follows, so it demands triage
// before code: a leftover Suggestion is often an artifact of the reviewers'
// isolated worktree (a missing service, an unprovisioned test database), and
// changing nothing is an explicitly allowed outcome.
func suggestionRoundPrompt(number int, branch string, branchOnly, offline bool, comment string) string {
	finish := "If you changed code: run the affected checks, commit, and push. Do not wait for CI; the pipeline watches the checks for you."
	finishBranch := "If you changed code: run the affected checks, commit, and push."
	if offline {
		finish = "If you changed code: run the affected checks and commit. Do not push."
		finishBranch = finish
	}
	if branchOnly {
		return fmt.Sprintf(`The final external code review of branch %s found no Blockers and no Critical issues, but it still lists Suggestions. The full review report follows below. This is the last step of the run and no further review will check your work, so be conservative.

For each Suggestion, first decide whether it is worth implementing. Skip it when it describes intended behavior, stems from the reviewers' isolated environment (for example a missing service or an unprovisioned test database), or its risk outweighs its value. Implement the ones that clearly improve the code. Changing nothing is a perfectly fine outcome.

%s If you changed nothing, do not commit or push, and briefly state per Suggestion why you skipped it.

---

%s`, branch, finishBranch, comment)
	}
	return fmt.Sprintf(`The final external code review of PR #%d found no Blockers and no Critical issues, but it still lists Suggestions. The full review comment follows below. This is the last step of the run and no further review will check your work, so be conservative.

For each Suggestion, first decide whether it is worth implementing. Skip it when it describes intended behavior, stems from the reviewers' isolated environment (for example a missing service or an unprovisioned test database), or its risk outweighs its value. Implement the ones that clearly improve the code. Changing nothing is a perfectly fine outcome.

%s Then end your final message with a line that is exactly:
%s
followed by a short log comment for the PR: what you changed and how, which checks you ran, and which Suggestions you left unchanged and why. Write it in the language of the PR description, plain and factual, and never mention AI, agents, or automation in it. Use this marker for nothing else.

If you changed nothing, do not commit or push, do not use the marker, and briefly state per Suggestion why you skipped it.

---

%s`, number, finish, MarkerComment, comment)
}

// conflictFixPrompt asks the session to merge the base branch and resolve the
// conflicts. The direction is base into head: it keeps the branch's own
// history intact and is the merge GitHub would attempt for the PR.
func conflictFixPrompt(number int, base, branch string, branchOnly, offline bool) string {
	target := fmt.Sprintf("PR #%d", number)
	if branchOnly {
		target = "branch " + branch
	}
	finish := ", and push."
	if offline {
		finish = ". Do not push."
	}
	prompt := fmt.Sprintf(`%s conflicts with its base branch %s and cannot be merged as it is.

Merge the base into the current checkout with exactly: git merge origin/%s
Resolve every conflict so that the intent of both sides survives; do not blindly take one side. Where base and branch changed the same behavior, prefer the base's version of unrelated changes and keep this branch's version of what the branch is about. After resolving, run the checks affected by the conflicting files, complete the merge with a clean commit message such as "Merge %s into %s"%s`,
		target, base, base, base, branch, finish)
	if branchOnly {
		return prompt
	}
	return prompt + fmt.Sprintf(`

End your final message with a line that is exactly:
%s
followed by a short log comment for the PR: which files conflicted and how the conflicts were resolved. Write it in the language of the PR description, plain and factual, and never mention AI, agents, automation, or the name of any coding tool in it. Use this marker for nothing else.`,
		MarkerComment)
}

// commitPrompt asks the session to commit what it left in the working tree.
const commitPrompt = `There are uncommitted changes in the worktree. Review them and commit everything that belongs to the task (git add + git commit with a descriptive message). Move disposable QA artifacts that you created to $TMPDIR or /tmp, or delete them. Do not push. Finish with an empty git status --porcelain.`

// bounceQuestionsPrompt sends open questions back in autonomous mode.
const bounceQuestionsPrompt = `No human is available during this automated run. Decide these questions yourself: pick the most conservative reasonable option for each, apply it, and record the decision in the PR comment section of your final message. Continue with the current step. All rules from the initial instructions still apply.`

// finalDescriptionPrompt asks a fresh read-only session to replace the PR body
// with a description of the finished diff. The original body arrives on stdin:
// it is evidence of intent, not a template that must preserve stale claims.
func finalDescriptionPrompt(number int, title, base string) string {
	return fmt.Sprintf(`Write the final Markdown description for PR #%d, titled %q. The original PR description is on stdin. Inspect the finished local diff against origin/%s and the relevant code.

Describe the PR as it exists now. The result must stand alone for a reviewer who has not seen its development. Preserve still-relevant issue links, rollout notes, test instructions, screenshots, and useful structure from the original description. Correct stale claims and cover material behavior present in the final diff. Keep it concise and use the language of the original description; if it is empty, use the language of the PR title.

Do not write a changelog or development narrative. Do not mention that the description was rewritten or updated. Do not mention review rounds, findings, fix steps, comments, agents, AI, automation, any coding tool, or how the implementation evolved. Do not invent test results or product decisions.

Compare the original stated intent with the finished implementation. If the final behavior, scope, or architecture materially departs from that intent, put one short blockquote at the very top with a calm warning label in the description's language, one sentence naming the difference, and one sentence asking for review before merge. Use this only for a material directional change, not ordinary refinements, bug fixes, or implementation details. Do not exaggerate.

Return only the complete Markdown PR description. Do not wrap it in a code fence and do not post or edit the PR yourself.`, number, title, base)
}

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

func branchContext(branch, base, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Branch: %s (base: %s)\nNo open pull request exists for this branch.", branch, base)
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

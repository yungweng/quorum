# Parity with the three tools quorum replaces

quorum is a port of `pr-codex-review` 1.6.0, `babysit` 0.6.2 and `prbot` 0.5.1.
The two shell tools were 2175 lines of Bash whose safety behaviour was spread
across the code and their CLAUDE.md files. This is the list that behaviour was
checked against, so a rewrite could not quietly drop it.

Every "kept" line has a test or is exercised by the type system. Every "changed"
line is a deliberate decision with its reason, not an accident of the port.

## Safety stops: kept

| Behaviour | Was | Now |
|---|---|---|
| Refuse to post when the PR head moved during the review | `pr-codex-review:770`, exit 3 | `review.ErrHeadDrifted`, exit 3 |
| Note it on the comment when only the base moved | `pr-codex-review:776` | `Runner.checkDrift` returns the note |
| Refuse automatic `direnv allow` when the PR changed an `.envrc` | `pr-codex-review:441`, exit 2 | `review.ErrEnvrcChanged`, exit 2 |
| Refuse to post a fenced or meta aggregator answer | `pr-codex-review:849` | `review.ValidateComment` |
| Require all five headings, retry once, then fail | `pr-codex-review:861,915`, exit 4 | `ValidateComment` + `Runner.aggregate`, exit 4 |
| Require bullets or a bare "None." per section | `pr-codex-review:877` | `sectionContentOK` |
| Require a minimum number of reviewer outputs | `pr-codex-review:756` | `review.ErrTooFewReviewers` |
| Delete a failed reviewer's partial output | `pr-codex-review:475` | `runReviewers` |
| Keep the worktree on failure so `--resume-run` works | `pr-codex-review:402` | `Runner.Run`, cleanup only on success |
| Refuse a dirty checkout of the PR branch | `babysit:379` | `run.prepare` |
| Refuse a local branch that differs from origin | `babysit:390` | `run.prepare` |
| Refuse fork PRs | `babysit:311` | `run.prepare` |
| Refuse a PR that is not open | `babysit:301` | `run.prepare` |
| Stop when a fix round produced no commits and no dispute | `babysit:1114`, exit 5 | `loop.ErrNoProgress`, exit 5 |
| Never accept a dispute on first sight | `babysit:755` | `run.disputeGate`, forced re-check |
| Bounce open questions back at most 3 times, then stop | `babysit:703`, exit 2 | `maxQuestionBounces`, exit 2 |
| Abort interactive gates without a terminal | `babysit:683`, exit 2 | `run.scanner`, exit 2 |
| Wait for GitHub to report the pushed head before reading checks | `babysit:830` | `run.pushBranch` |
| Verify `findings.json` head matches the branch | `babysit:1073` | `run.execute` |
| Wait out (not kill) a review a CI fix invalidates | `babysit:962` | `run.discardReview` |
| Kill a running review on the paths that end the run | `babysit:965` | `run.killReview` |
| Only Blockers and Critical keep the loop alive | `babysit:1078` | `Findings.Blocking` |
| Commit leftover changes before judging progress | `babysit:808` | `run.ensureCommitted` |
| The pipeline posts the fix log, never Codex | `babysit:862` | `run.postFixComment` |
| Never pass `--allow-envrc-change` for an automated review | `prbot README` | `Runner.reviewOptions` |
| Skip drafts, forks, bots and your own PRs | `prbot poll.go` | unchanged |
| A failed run leaves the request unrecorded so it retries | `prbot runner.go` | unchanged |

## Deliberate changes

Each of these is a place where the port does something different on purpose.

1. **Shared dependency trees are no longer deleted while in use.**
   `pr-codex-review:365` deleted trees with `find … -type d -mtime +14`, but the
   "touch on use" at line 617 touched `$shared/.complete`, a file *inside* the
   directory. Writing a file does not change its parent's mtime, so the
   directory kept the mtime it got from the `mv` that created it and every tree
   was deleted 14 days after creation no matter how often it was reused.
   Verified on macOS. `deps.GC` now reads the marker's mtime, which is what the
   original comment said it was doing.

2. **Process groups instead of walking `ps`.** `kill_tree` recursed through `ps`
   output killing pid by pid, which races: a child spawned between the walk and
   the kill survives. Commands now lead their own process group and one signal
   to the negative pid reaches every descendant. Codex spawns MCP servers and
   toolchains below itself, so one survivor keeps doing whatever it was doing.

3. **The aggregator has a timeout.** `pr-codex-review:916` ran it with none, so
   a hung aggregation stalled the run forever. It gets the reviewer timeout.

4. **The Codex session id comes from the rollout JSON.** `babysit:568` parsed it
   out of the file name with a UUID regex. The first line of the file contains
   `payload.session_id` directly (verified against Codex CLI 0.145.0). The
   matching rule is unchanged and still the safety argument: the worktree path
   is unique per run, so a concurrent session elsewhere cannot match.

5. **One cache tree.** `~/.cache/pr-codex-review`, `~/.cache/babysit` and
   `~/.cache/prbot` become `~/.cache/quorum/{reviews,babysit,deps,repos}`. The
   shared dependency cache is now a sibling of the run directories rather than
   inside them, so the run GC no longer needs an exception to step over it.

6. **`REVIEW_ARGS` is retired.** It was passed verbatim to a binary that no
   longer exists. `--dry-run` in an existing config still turns posting off, the
   old value is preserved so it can be read, and it is written back commented
   out. Replaced by `REVIEW_MODEL`, `REVIEW_EFFORT`, `REVIEWERS` and `POST`.

7. **The fetch ref is `refs/quorum/pr/<n>`**, was `refs/pr-codex-review/<n>`.
   Internal to the tool; old refs are harmless leftovers.

8. **The agent can babysit.** `AGENT_ACTION=babysit` makes the daemon run the
   full fix loop instead of posting one review. This was impossible before,
   because the daemon and the pipeline were separate binaries.

## What the merge removed rather than ported

- **stdout scraping.** `prbot/internal/runner/runner.go:214` grepped the review's
  stdout for a `Run dir` line to find its output. It is a return value now.
- **Run directory guessing.** `babysit:948` diffed directory listings of the
  cache to work out which run it had just started, with a documented fallback
  for "could not identify exactly one". Also a return value now.
- **The version handshake.** babysit required pr-codex-review >= 1.2.0 for
  `findings.json`. One binary, one version.
- **Three Homebrew formulas** and the `pr-codex-review` runtime dependency.

## Not yet verified against a live PR

The port compiles, passes its tests and its own linter, but the end-to-end path
that actually spends Codex tokens has not been run. Worth doing first, in this
order, on a PR you do not mind:

1. `quorum review <pr> --dry-run -n 2` — reviewer fan-out, dependency cache,
   aggregation and validation, without posting.
2. `quorum review <pr> -n 2` — posting and `findings.json`.
3. `quorum babysit <pr> --max-iter 1` — the fix session, session recovery, the
   push barrier and the fix-log comment.

# Reference

Every option, setting, exit code and file. [The README](../README.md) covers
what quorum does; this covers what you can set.

Running `quorum` without a command shows the command overview. The three main
commands are `quorum watch`, `quorum review` and `quorum babysit`.

## quorum review

```text
quorum review [pr-number|github-pr-url] [options]
```

Without a PR argument, quorum uses the open PR for the current branch when one
exists. Otherwise it reviews the pushed branch against the repository default
branch and writes the report without posting. The checkout must be clean and
match `origin`. Run the command from inside that repository's checkout.

| Option | Meaning | Default |
|---|---|---|
| `-n`, `--runs N` | Reviewer passes | 6 |
| `--concurrency N` | Reviewer passes at once | same as `--runs` |
| `--engine ENGINE` | `codex`, `claude` or `grok` | `codex` |
| `--model MODEL` | Model for reviewers, aggregator and verifier | engine default: `gpt-5.6-luna` (codex), `sonnet` (claude), `grok-4.5` (grok) |
| `--effort LEVEL` | codex: `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra`; claude: `low`, `medium`, `high`, `xhigh`, `max`; grok: `low`, `medium`, `high` | `max` (codex), the engine's own (claude, grok) |
| `--base BRANCH` | Base branch to review against | PR base or repository default |
| `--dry-run` | Write the report to disk without posting it | off |
| `--keep-worktree` | Keep the worktree after a successful run | off |
| `--resume-run DIR` | Reuse a run directory with its original target and base | |
| `--review-timeout DUR` | Kill a reviewer that runs too long | 45m |
| `--min-successful N` | Reviewer outputs required to post | a majority |
| `--no-direnv` | Skip direnv | off |
| `--allow-envrc-change` | Allow `direnv allow` when the target changed `.envrc` | off |
| `--no-notify` | Disable completion and action notifications | off |
| `-h`, `--help` | Show the help | |

`--post`, `--cleanup` and `--allow-base-drift` are accepted and do nothing. They
describe the default behaviour now, and are kept so old invocations do not break.

## quorum babysit

```text
quorum babysit [options] [pr-number|pr-url] [extra context...]
```

Options and positionals may interleave. Extra positional text becomes context
for the fix session. Without a PR argument, quorum uses the current branch's
open PR when one exists. With no open PR it works on the clean, pushed branch,
runs repository checks in each fix step, confirms pushes on `origin`, and skips
PR CI and PR comments. `--local` forces that branch mode even when an open PR
exists, so a run can stay off the PR entirely.

By default the loop runs offline (`LOOP_MODE=offline`): every review-fix round
works on local commits in the run's worktree, reviews check the unpushed head,
and the per-repo test command guards each round instead of a CI wait. Only a
converged run pushes - once - which triggers a single CI run; that is what
keeps a babysit run from billing one GitHub Actions run per fix round. During
the offline rounds nothing is posted to the PR; after the final push quorum
posts one consolidated fix-log comment covering all rounds plus the final
review's comment. When CI repairs move the head after that push, one more
review round checks the repaired head before the run converges. If someone
else pushes to the branch while the loop iterates locally, the final push
fails as non-fast-forward and the run stops instead of overwriting their work.
`LOOP_MODE=online` or `--online` restores the old behaviour: push and wait for
CI after every fix round.

The test command is resolved in this order: `--test-cmd`, the user-local
`~/.config/quorum/testcmd/<owner>/<repo>` (a personal override), then the
repository's own tracked `.quorum/testcmd`, so a team shares one gate by
committing that file. The repo file is deliberately read from the base branch
(`origin/<base>`), never from the change under review: the command runs
unsandboxed on this machine, and reading it out of the diff would let the
change under babysit weaken or hijack its own gate - the same reasoning as the
`.envrc` stop. A change that edits `.quorum/testcmd` therefore still runs
against the old gate; the new one applies once it is merged. The command runs
through direnv in the worktree; a red run feeds the output back into the fix
session, up to `--max-ci-fixes` attempts. Without any configured command the
fix sessions are still told to run the affected checks themselves, but nothing
verifies it.

When a push is refused before it reaches the remote, which is what happens in
a repository whose pre-push hooks verify the commits, the hook output goes to
the fix session and the push is retried, at most twice. This covers the checks
a local test gate does not run - type checks, unused exports, dead code - and
which would otherwise end a converged run with nothing pushed. The session
repairs the code and commits; the pipeline keeps the push, the test gate runs
again over the repair, and a fix that edits the hook configuration instead of
satisfying it stops the run. Rejections git describes itself - non-fast-forward,
credentials, a protected branch, the network - never reach a fix session.

Draft PRs are refused unless the run says `--draft` or the config says
`BABYSIT_DRAFTS=1`. When the branch conflicts with its base branch, the base is
merged and the conflicts resolved through the fix session before the first
review; `RESOLVE_CONFLICTS=0` or `--no-resolve-conflicts` turns that off.

When the final review comes back with zero Blockers and Critical findings but
still lists Suggestions, one last fix round triages them in the same session:
it implements the ones worth keeping, skips the ones that describe intended
behavior or stem from the reviewers' isolated worktree, and no further review
follows. A round that changes nothing ends the run exactly like a clean review
did before. `FIX_SUGGESTIONS=0` or `--no-fix-suggestions` turns the round off.
If it pushes commits, the run still waits for CI on them, and auto-merge is
skipped because the review never saw those commits.

After a posted PR run converges with green CI, a fresh read-only Codex pass
writes a local PR-description candidate. The result describes the final
implementation, not the sequence of findings and fixes. It keeps relevant
links, rollout notes, test instructions and screenshots. When the final
behavior, scope or architecture materially departs from the original direction,
it adds one short warning at the top. Refinements and bug fixes do not trigger
that warning. Immediately before writing, quorum re-reads the PR and rejects
the candidate if the description or head changed during the run; otherwise it
replaces the remote description. `POST=0`, `--local` and branch-only runs skip
generation.

| Option | Meaning | Default |
|---|---|---|
| `--engine ENGINE` | Engine for the fix sessions: `codex`, `claude` or `grok` | `codex` |
| `--model MODEL` | Model for the fix sessions | the engine's own default |
| `--effort LEVEL` | codex: `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra`; claude: `low`, `medium`, `high`, `xhigh`, `max`; grok: `low`, `medium`, `high` | the engine's own default |
| `--reviewers N` | Reviewer passes per review round | 6 |
| `--review-engine ENGINE` | Engine for the review rounds | `codex` |
| `--review-model MODEL` | Model for the review rounds | engine default: `gpt-5.6-luna` (codex), `sonnet` (claude), `grok-4.5` (grok) |
| `--review-effort LEVEL` | Effort for the review rounds | `max` (codex), the engine's own (claude, grok) |
| `--max-iter N` | Review to fix rounds before giving up | 12 |
| `--max-ci-fixes N` | CI or local test fix attempts per green phase | 3 |
| `--fix-timeout DUR` | Kill a fix step or test run that runs longer | 2h |
| `--offline` | Iterate locally, push once at the end | on, `LOOP_MODE=offline` |
| `--online` | Push and wait for CI after every fix round | off, `LOOP_MODE=online` |
| `--test-cmd CMD` | Shell command the offline loop runs as its test gate | `~/.config/quorum/testcmd/<owner>/<repo>`, else `.quorum/testcmd` on the base branch, else none |
| `--divergence-scan` | Analyze all rounds after the limit, write a report, then stop | off |
| `--draft` | Work on a draft PR | off, or `BABYSIT_DRAFTS=1` |
| `--local` | Ignore any open PR and work on the pushed branch only | off |
| `--no-resolve-conflicts` | Do not merge the base branch on conflicts | resolution on |
| `--no-fix-suggestions` | Skip the suggestion triage round after a clean review | round on |
| `--sandboxed` | Use the engine's own sandbox and approval defaults | off |
| `--interactive` | Ask at gates instead of deciding autonomously | off |
| `--verbose` | Stream the full output instead of the status line | off |
| `--no-notify` | Disable completion and action notifications | off |
| `--no-direnv` | Skip direnv | off |
| `--allow-envrc-change` | Allow `direnv allow` when the target changed `.envrc` | off |
| `--keep-worktree` | Keep the worktree after success | off |
| `-h`, `--help` | Show the help | |

Failed runs always keep their worktree for inspection, regardless of
`--keep-worktree`. The normal cache collector removes old failed runs after the
retention period.

## Other commands

| Command | Options |
|---|---|
| `quorum` | Show the command overview |
| `quorum watch` | Follow running, queued and finished work |
| `quorum status` | Show one dashboard snapshot |
| `quorum logs` | `-n N` or `--lines N` for the tail length (default 50), `--no-follow` to print it and stop. A bare number also sets the length. |
| `quorum gc` | `--dry-run` or `-n` to report what would go without removing it |
| `quorum doctor` | `--fix` to apply what it can |
| `quorum config` | `--path` or `-p` to print the config file location |

## Engines

Reviews and fix sessions each run on one of three engines, selected with
`REVIEW_ENGINE`/`FIX_ENGINE` in the config or `--engine`/`--review-engine` on
the command line: `codex` (OpenAI's Codex CLI, the default), `claude`
(Anthropic's Claude Code CLI) or `grok` (xAI's Grok CLI). An empty model or
effort means the selected engine's own default: `gpt-5.6-luna` at effort
`max` for codex reviews, `sonnet` for claude and `grok-4.5` for grok, whose
own efforts are left alone unless you set one. The engines take a reasoning
effort, but not the same levels: codex accepts `minimal` through `ultra`,
claude accepts `low` through `max`, grok accepts `low` through `high`. Some
CLIs fall back to a default on an unknown level instead of failing, so
quorum refuses a level passed on the command line that the selected engine
cannot use instead of starting the run. A level already in the config is
dropped rather than refused: `REVIEW_EFFORT="ultra"` next to
`REVIEW_ENGINE="claude"` runs at claude's own effort, which the run header
names, instead of stopping every review including the agent's. The claude
and grok engines' review passes run with a fixed read-only tool set; claude
also turns off MCP and session persistence, while grok uses a read-only
sandbox profile and blocks subagents.

## It runs unattended

The fix sessions bypass the engine's sandbox and approvals by default: codex
runs with `--dangerously-bypass-approvals-and-sandbox`, claude with
`--dangerously-skip-permissions`, grok with `--always-approve`. They have to:
they run tests, use `gh` and `git push`, all with nobody watching, and a
sandboxed session would silently skip exactly those commands. Know what that
means. An agent with full file and network access on your machine, for up to
`--fix-timeout` per step.

`--sandboxed` opts out, but then the engine's own configuration must allow
commands, network and push or every fix round fails. Reviewers, the aggregator
and the verifier remain read-only. A separate Git integrity gate rejects any
changed HEAD or staged, unstaged or untracked file after verification. One
exception: tracked files the environment setup itself rewrote before any
reviewer ran (devbox regenerating its lock file, typically) are recorded with
their exact content and tolerated by that gate; different content in those
files, or any other tracked change, still fails the run.

## Usage limits

When the engine refuses to run because the account's usage limit is exhausted,
`quorum review` and `quorum babysit` stop with exit code 8 and the reset time
from the refusal. The agent writes a pause marker to its state directory and
starts no new reviews until that time (or one hour, when no reset time could
be read); pending requests stay queued and are picked up by the first poll
after the pause, without counting against `MAX_RETRIES`. A run whose reviewers
finished before the limit struck keeps its run directory marked resumable, so
the retry reuses the completed reviewer output instead of paying for a fresh
fan-out.

## Safety stops

These are deliberate refusals. Each one exists because the alternative is
posting or committing something misleading.

### quorum review

- **The target head moved during the review.** The run refuses to publish stale
  findings. If only the base moved, the run continues and the report carries a
  note.
- **The target changed an `.envrc`.** `direnv allow` executes whatever is in that
  file. The run stops unless you pass `--allow-envrc-change` after reading the
  diff yourself.
- **The aggregator produced the wrong shape.** The comment must have exactly
  five sections with findings as bullets. A renamed heading or a finding written
  as prose would count as zero findings and make a PR with real blockers report
  itself clean, so the run retries once and then fails instead of posting.
- **The evidence verifier failed or changed the repository.** A fresh pass
  checks each aggregated finding against the diff and code. It may preserve,
  correct, remove or add findings when the evidence supports that result. It
  runs in a read-only sandbox, and Go checks the pinned HEAD and full porcelain
  status before and after every attempt. Any mutation stops immediately. A
  timeout or malformed report gets one retry, then the run stops before posting
  or writing `findings.json`.

### quorum babysit

- **Draft PRs are refused** unless you pass `--draft` or set `BABYSIT_DRAFTS=1`.
  A draft is a PR its author marked "not ready"; pushing fix commits and posting
  comments to it needs an explicit go-ahead.
- **A conflict resolution that did not resolve stops the run.** After the merge
  session, the same conflict probe runs again; a merge that left conflict
  markers or skipped a file cannot pass on the session's own say-so.
- **The target changed an `.envrc`.** The run stops before loading it unless
  you pass `--allow-envrc-change` after reading the diff yourself.
- **Do not push to the target branch while a run is active.** A review refuses
  to use stale findings when the head moves under it, and the pipeline treats
  that as fatal.
- **The final PR-description update is guarded against stale input.** It checks
  the PR head and exact description before generation and again immediately
  before the write. If either changed, the run rejects the candidate and keeps
  it local. The generator runs in a fresh read-only session without the fix
  history or sandbox bypass; any local Git mutation, empty body, fenced output
  or oversized body also stops the run. GitHub provides no conditional PR-body
  update, so the final re-read directly before the write is what protects a
  human edit and the target head; the remaining race window is one API call.
- The pipeline refuses to start on a dirty checkout of the target branch, or when
  your local branch carries commits origin does not have: it reviews the pushed
  head and would otherwise silently ignore your work. A local branch that is
  merely behind origin (routine after any run's own pushes) proceeds with a
  note; the run works on origin's head either way.
- Fork PRs are refused: the pipeline pushes `refs/heads/<branch>` on origin,
  which for a fork PR is the wrong branch.
- After every push it waits until GitHub reports the new head before reading any
  check result, because `gh pr checks` briefly still answers for the old one and
  a red commit would read as green.
- A fix round that produces no commits while findings remain stops the run.

## Exit codes

The same numbers mean different things per command, because both inherited them
from the tools they replace.

`quorum review`:

```text
0  posted for a PR, or written to disk for --dry-run or a branch without a PR
1  any other failure
2  refused: the target changes an .envrc
3  refused: the target head moved during the review
4  the aggregator or verifier could not produce a valid report
8  the engine refused: its usage limit is exhausted
```

`quorum babysit`:

```text
0  review clean (and CI green for a PR), or remaining findings disputed and accepted
1  any other failure
2  aborted at a gate
3  CI or the local test command still red after --max-ci-fixes attempts
4  not converged after --max-iter rounds
5  a fix round produced no changes although findings remain
6  the review/fix history contains incompatible decisions
7  merge conflicts with the base branch remain unresolved
8  the engine refused: its usage limit is exhausted
```

## Configuration

`quorum config` shows every setting and lets you change it. The file is plain
`KEY=value` at `~/.config/quorum/config`.

```bash
# Scope. Empty means every review request assigned to you, anywhere.
ORGS=""
REPOS=""
EXCLUDE_REPOS=""
INCLUDE_TEAMS=1
TEAMS=""                 # empty discovers your teams

# How much runs at once. Every review runs all REVIEWERS passes in parallel,
# so at peak there are MAX_CONCURRENT x REVIEWERS Codex processes.
MAX_CONCURRENT=6
REVIEWERS=6
NICE=10                  # reviews give way to your own work
LOAD_LIMIT=0             # hold reviews back above this load, 0 disables
CACHE_BUDGET_GB=5        # runs and dependency trees together, 0 disables

MAX_RETRIES=3
POLL_INTERVAL=300        # next poll reloads the agent job when this changes
HISTORY=20               # finished runs listed by status and watch

# Safety. Fork and bot PRs run foreign code locally through direnv.
SKIP_DRAFTS=1
SKIP_FORKS=1
SKIP_BOTS=1
SKIP_OWN=1               # review one of your own with `quorum run`

# Reviews. Empty model/effort means the engine's default: gpt-5.6-luna at
# effort max for codex, sonnet for claude, grok-4.5 for grok.
REVIEW_ENGINE="codex"    # or "claude" or "grok"
REVIEW_MODEL=""
REVIEW_EFFORT=""         # codex: minimal..ultra, claude: low..max, grok: low..high
REVIEW_TIMEOUT="45m"     # per reviewer pass, not per run. 0 disables
POST=1                   # 0 disables PR comments and final description generation

# Babysit
FIX_ENGINE="codex"       # or "claude" or "grok"
FIX_MODEL=""             # empty leaves the choice to the engine's own CLI
FIX_EFFORT=""            # codex: minimal..ultra, claude: low..max, grok: low..high
MAX_ITER=12
MAX_CI_FIXES=3
FIX_TIMEOUT="2h"         # per fix step; keep it above your CI runtime
DIVERGENCE_SCAN=0        # analyze the current run after MAX_ITER, then stop
DIVERGENCE_ESCALATE_TO="" # users or org/team slugs to mention, without @
SANDBOXED=0
BABYSIT_DRAFTS=0         # 1 lets quorum babysit work on draft PRs without --draft
RESOLVE_CONFLICTS=1      # merge the base branch and resolve conflicts before reviewing
FIX_SUGGESTIONS=1        # after a clean final review, triage and implement leftover Suggestions once
LOOP_MODE="offline"      # offline: iterate locally, one push and CI run at the end; online: push and CI wait every round

AGENT_ACTION="review"    # or "babysit"
AUTO_MERGE=0             # one switch for agent, review and babysit runs
AUTO_MERGE_TIMEOUT="2h"  # wait for checks and mergeability; 0 disables timeout
AUTO_MERGE_AUTHORS=""    # only merge PRs from these logins; empty allows every author
NOTIFY=1                 # terminal; macOS system alerts need terminal-notifier
NOTIFY_READY_TO_MERGE=0  # persistent alert per PR whose clean review was not auto-merged
```

Durations accept a bare number of seconds or a value like `30m`, `45m`, `2h`.
Zero disables the timeout it belongs to.

`REVIEW_ARGS` and `MAX_CODEX` are retired. A `REVIEW_ARGS` that contains
`--dry-run` still turns posting off; nothing else in it is read.

### Auto-merge

`AUTO_MERGE` defaults to `0` and is one switch for every source: agent runs,
`quorum review`, `quorum run` and `quorum babysit`. The retired per-source keys
are still read for migration: `AUTO_MERGE_AGENT` carries its value over,
`AUTO_MERGE_REVIEW` and `AUTO_MERGE_BABYSIT` are ignored, and all three are
removed when the file is rewritten.

`AUTO_MERGE_TIMEOUT` defaults to two hours. Set it above the longest protected
check or merge queue wait in the repository. A value of `0` waits until the run
is stopped.

`AUTO_MERGE_AUTHORS` is a space-separated whitelist of GitHub logins, compared
case-insensitively. When it is set, only pull requests from those authors are
approved and merged; a clean review of anyone else's PR is posted as usual, but
the merge is skipped and reported as `awaiting approval`, exactly like an own
PR. An empty list keeps the old behaviour and allows every author.

After a posted review with zero Blockers and zero Critical findings, quorum:

1. confirms that GitHub still reports the exact reviewed head;
2. submits an approval tied to that commit, unless the same user already
   approved it; and
3. reads the repository's allowed merge methods and calls GitHub's merge API
   with `sha=SHA`, preferring `merge`, then `squash`, then `rebase`.

Suggestions and Questions do not block. GitHub branch rules and required checks
still apply; optional check failures do not block the merge wait. The merge is
one atomic request for the reviewed SHA, so it fails rather than leaving a new
request that could survive a later push. Target branches that require a merge
queue are rejected before approval. Quorum never
disables an existing auto-merge or merge-queue request because it cannot prove
who created it. Repositories that allow none of GitHub's merge, squash, or
rebase methods are rejected before approval. For an own PR, quorum skips
approval and merge, reports `awaiting approval`, and leaves a per-PR macOS
Notification Center item that
routine completion notifications cannot replace. It does not merge a moved
head, a branch-only run, `POST=0`, `--dry-run`, or an accepted dispute whose
last review still contains Blockers or Critical findings.

If branch requirements are still pending, Auto-Merge fails instead of queuing a
persistent request. An Auto-Merge failure returns exit code 1. An own PR that
is awaiting approval is a successful review, not an Auto-Merge failure. Agent
runs record the review request as handled, so a failure after a successful
review does not spend tokens repeating that review.

### Ready-to-merge notifications

`NOTIFY_READY_TO_MERGE=1` leaves one persistent macOS Notification Center item
per pull request whose review came back with zero Blockers and zero Critical
findings but was not merged by quorum, typically because auto-merge is off.
The notification names `owner/repo#number`; clicking it opens the
pull request on GitHub. It replaces the routine completion notification for
that run and stays visible until dismissed, like the `awaiting approval` item.
It requires `terminal-notifier` and `NOTIFY=1`, obeys `--no-notify`, and never
fires for a merged PR, a branch-only run, `POST=0`, or `--dry-run`. The
default is `0`.

### Keeping the machine usable

The Codex reviewers spend most of their time waiting on the network. What used
to cost real time was the dependency install, once per reviewer, which the
shared dependency cache removed.

- `MAX_CONCURRENT` is how many reviews run at once, and each runs all
  `REVIEWERS` passes in parallel. A review that trickles through its passes is a
  review you end up waiting for. `quorum doctor` warns when the peak would take
  more than half the machine's memory.
- `NICE` lowers the priority of the whole review process tree.
- `LOAD_LIMIT` is off by default. Turned on, it holds new reviews back while the
  machine is busy; they start on a later poll.
- `HISTORY` is how many finished runs the dashboard reads. It counts runs, not
  pull requests, so four reviews of the same one are four of the twenty; they
  share one line on screen, which says how many they were.

## The dashboard

`status` draws it once, `watch` redraws it every three seconds. Three sections,
in the order the questions get asked:

- **OPEN** lists pull requests with a finished review that are still open,
  newest first, at most ten of them, with what the review found and a link to
  the comment. `auto-merge queued` means GitHub is waiting on branch rules or a
  merge queue; the other entries still need a person. A pull request drops out
  when GitHub reports it merged or closed, when GitHub no longer exposes its
  repository or PR, while it is being reviewed again, and two weeks after its
  last review. The two weeks matter because the state file keeps two hundred
  records and most of them describe pull requests that were merged long ago;
  where GitHub has not been asked, age is the only thing the dashboard knows.
- **ACTIVE** is everything in flight, agent and terminal alike. Its count covers
  agent slots only.
- **HISTORY** is one line per pull request, newest first, reporting its newest
  run. Where more than one run went into it, the line says how many there were
  and how many failed, so a failure a later run fixed is not hidden by it. The
  log behind the section is described below.

All three sections draw the same columns at the same widths, measured from
every row of the frame before any of it is printed. What a narrow terminal
costs is fixed rather than shared out: the author column goes first and goes
whole, then the explanation behind a result, and the repository and number
last. A column that cannot keep a useful width is dropped rather than cut to a
stub, on the grounds that a column which is gone is understood as gone.

Whether a pull request is open, queued for Auto-Merge, merged or closed has to
come from GitHub. `watch` asks in the background, once for every visible pull
request at a time, and keeps the previous answer when the call fails. `status`
asks once before it draws, with a single attempt and a five second deadline,
and falls back to listing everything recent when there is no answer: this is
the only network call either command makes.

## The history log

`status` and `watch` list what has finished under HISTORY, read from
`~/.local/state/quorum/history.jsonl`. One line of JSON per run, oldest first,
trimmed to the newest 500.

It exists because neither of the other stores can answer "what has quorum
done". The state file keeps one record per pull request, so a second review of
the same one overwrites the first, and only the agent writes to it, so a
`quorum review` or `quorum babysit` started in a terminal never appears. The
run cache does keep every run, but its directory names flatten the slash in a
repository name into a hyphen, which cannot be parsed back, and it is collected
once `CACHE_BUDGET_GB` is reached.

Every finished run appends one entry: the agent's through the same state write
that records the outcome, and a terminal run when its command returns. A skip
is recorded only when the decision is new, because the poll re-applies it on
every pass. Deleting the file loses the list and nothing else.

## findings.json

Every successful run writes `findings.json` beside the comment, so scripts and
the fix pipeline never have to parse Markdown.

```json
{
  "schema": 1,
  "pr": 1811,
  "head_sha": "…",
  "base_sha": "…",
  "reviewers_succeeded": 6,
  "reviewers_requested": 6,
  "blockers": 0,
  "critical": 2,
  "suggestions": 3,
  "questions": 1,
  "comment_file": "…/output/final-pr-comment.md",
  "posted": true,
  "comment_url": "https://github.com/owner/repo/pull/1811#issuecomment-…"
}
```

`comment_url` is null when nothing was posted, which is every `--dry-run` and
every run with `POST=0`. The four counts come from the verified report's bullets
under the `## Blockers`, `## Critical`, `## Suggestions` and `## Questions`
headings. `aggregated-pr-comment.md` keeps the pre-verification candidate,
`verification-changes.md` lists only removed, rewritten or added finding blocks,
and `verifier.log` records the verifier run. These are local audit artifacts;
only `final-pr-comment.md` is posted.

A successful posted `quorum babysit` also writes
`messages/final-pr-description.md`, the final PR body. Quorum updates the
remote description from it after a last drift check, reports when the remote
body already matched, and keeps the file local when that check or the update
fails.

## Files

```text
~/.config/quorum/config                        configuration
~/.local/state/quorum/state.json               every PR the agent has seen
~/.local/state/quorum/history.jsonl            every run that has finished
~/.local/state/quorum/quorum.log               what it did and why it skipped things
~/.local/state/quorum/running/                 one marker per review in flight
~/.local/state/quorum/runs/                    one log per agent run
~/.cache/quorum/reviews/                       review runs and their output
~/.cache/quorum/babysit/                       babysit runs, logs and Codex messages
~/.cache/quorum/deps/                          shared dependency trees
~/.cache/quorum/repos/                         managed clones
~/.cache/quorum/trash/                         interrupted GC staging, normally absent
~/Library/LaunchAgents/io.github.quorum.plist  the launchd agent
```

`QUORUM_CONFIG`, `QUORUM_STATE_DIR` and `QUORUM_CLONE_DIR` override the first,
the state directory and the clone directory. The rest follow `XDG_CONFIG_HOME`,
`XDG_STATE_HOME` and `XDG_CACHE_HOME`.

### What gets cleaned up

A successful run deletes its own worktree, which is nearly all of what it took
up. A failed review keeps it for `--resume-run`; a failed babysit keeps it for
diagnosis. Run directories are dropped a week after anything last looked at
them, dependency trees after two weeks. `CACHE_BUDGET_GB` bounds the three
cache directories together and every poll enforces it, so nothing waits for
somebody to run `quorum gc`. The managed clones are outside the budget: one per
repository, bounded by how many you review.

The run cache never keeps a repository's `main`, `development` or other branch
checkout. Its worktrees are detached snapshots made for one run. Successful
runs remove those snapshots immediately; the directories that remain belong to
failed or explicitly retained runs, plus their review output.

Above the budget, collection removes inactive worktrees first, then old run
output, then shared dependencies. Active runs are protected by their claim
locks. Selected entries are atomically moved to `~/.cache/quorum/trash/` while
the short startup lock is held and recursively deleted after the lock is
released. Interrupted deletions resume on the next collection. Cache removal
also handles the read-only directories produced by Go's module cache and other
dependency managers.

### Shared dependency cache

Projects that install dependencies from a direnv or devbox hook would otherwise
pay for that install once per reviewer. quorum enters the environment once
before the reviewers start, moves what the hook installed to
`~/.cache/quorum/deps/<repo>/<project>/<lock-hash>/`, and symlinks it back in
for each run. The hook's own "already installed?" guard then sees a populated
directory and skips. Any change to the lock file produces a different hash, so a
stale tree is never reused.

Nothing to configure. `--no-direnv` skips the whole step, and removing a
worktree removes only the symlink, never the shared tree.

## Troubleshooting

**Nothing happens.** `quorum doctor` checks the tools, the GitHub login, the
agent and whether it has completed a poll recently, and prints the command to
fix what it found. Every skip also carries its reason in `quorum` output and in
the log.

**A GitHub call failed.** Transient failures are retried with a backoff and
reported as retries. They are never recorded as a decision not to review, so a
hiccup cannot make a pull request silently disappear from the queue.

**Works in the terminal, not from launchd.** Almost always `PATH`. launchd
ignores your shell profile, so the job carries a fixed agent `PATH` (the usual
install locations for `gh`, `git`, `codex`, `claude`, `grok` and `direnv`). A
tool installed only outside that set is invisible to the agent until its
directory is added or you put a symlink under one of those locations. When the
installed job no longer matches what the current binary would write (binary
path, poll interval, agent PATH or job template), the next poll rewrites and
reloads it; `quorum doctor --fix` does the same. An upgraded binary at the
same path needs no reinstall: launchd starts each poll fresh through that
path. First-time setup and removal stay explicit: `quorum install` and
`quorum uninstall`.

**gh cannot authenticate from the agent.** `gh` stores its token in the
keychain, which a user agent can only reach while your session is unlocked. Put
`GH_TOKEN` in a file with mode 600 and source it from the config if that is a
problem.

**A PR is stuck after failed reviews.** A failed run leaves the request
unrecorded so the next poll retries, up to `MAX_RETRIES`. After that it is
marked as given up. Retry with `quorum run owner/repo#123`. Giving up binds to
that request: a newer review request on the same pull request starts over with
a fresh retry budget.

**The review was posted against outdated code.** The review starts as soon as
the request appears. If the author keeps pushing, the run refuses to post
against a drifted head and fails, which the retry handles on the next poll.

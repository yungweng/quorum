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
| `-n`, `--runs N` | Codex reviewer passes | 6 |
| `--concurrency N` | Reviewer passes at once | same as `--runs` |
| `--model MODEL` | Model for reviewers and aggregator | `gpt-5.6-terra` |
| `--effort LEVEL` | `minimal`, `low`, `medium`, `high`, `xhigh` | `medium` |
| `--base BRANCH` | Base branch to review against | PR base or repository default |
| `--dry-run` | Write the report to disk without posting it | off |
| `--keep-worktree` | Keep the worktree after a successful run | off |
| `--resume-run DIR` | Reuse a run directory with its original target and base | |
| `--review-timeout DUR` | Kill a reviewer that runs too long | 45m |
| `--min-successful N` | Reviewer outputs required to post | a majority |
| `--no-direnv` | Skip direnv | off |
| `--allow-envrc-change` | Allow `direnv allow` when the target changed `.envrc` | off |
| `--no-notify` | No terminal notification when the run finishes | off |
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
PR CI and PR comments.

| Option | Meaning | Default |
|---|---|---|
| `--model MODEL` | Model for the fix sessions | your codex default |
| `--effort LEVEL` | `minimal`, `low`, `medium`, `high`, `xhigh` | your codex default |
| `--reviewers N` | Reviewer passes per review round | 6 |
| `--review-model MODEL` | Model for the review rounds | `gpt-5.6-terra` |
| `--review-effort LEVEL` | Effort for the review rounds | `medium` |
| `--max-iter N` | Review to fix rounds before giving up | 12 |
| `--max-ci-fixes N` | PR CI fix attempts per green-CI phase | 3 |
| `--fix-timeout DUR` | Kill a fix step that runs longer | 2h |
| `--divergence-scan` | Analyze all rounds after the limit, write a report, then stop | off |
| `--sandboxed` | Use your codex sandbox and approval defaults | off |
| `--interactive` | Ask at gates instead of deciding autonomously | off |
| `--verbose` | Stream the full output instead of the status line | off |
| `--no-notify` | Disable terminal notifications | off |
| `--no-direnv` | Skip direnv | off |
| `--allow-envrc-change` | Allow `direnv allow` when the target changed `.envrc` | off |
| `--keep-worktree` | Keep the worktree after success | off |
| `-h`, `--help` | Show the help | |

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

## It runs unattended

The fix sessions use `--dangerously-bypass-approvals-and-sandbox` by default.
They have to: they run tests, use `gh` and `git push`, all with nobody watching,
and a sandboxed `codex exec` would silently skip exactly those commands. Know
what that means. An agent with full file and network access on your machine, for
up to `--fix-timeout` per step.

`--sandboxed` opts out, but then your `~/.codex/config.toml` must allow
commands, network and push or every fix round fails. The review side is
unaffected and runs read-only.

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

### quorum babysit

- **The target changed an `.envrc`.** The run stops before loading it unless
  you pass `--allow-envrc-change` after reading the diff yourself.
- **Do not push to the target branch while a run is active.** A review refuses
  to use stale findings when the head moves under it, and the pipeline treats
  that as fatal.
- The pipeline refuses to start on a dirty checkout of the target branch, or when
  your local branch differs from origin: it reviews the pushed head and would
  otherwise silently ignore your work.
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
4  the aggregator could not produce a valid report
```

`quorum babysit`:

```text
0  review clean (and CI green for a PR), or remaining findings disputed and accepted
1  any other failure
2  aborted at a gate
3  CI still red after --max-ci-fixes attempts
4  not converged after --max-iter rounds
5  a fix round produced no changes although findings remain
6  the review/fix history contains incompatible decisions
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
POLL_INTERVAL=300        # re-run `quorum install` after changing this
HISTORY=20               # finished runs listed by status and watch

# Safety. Fork and bot PRs run foreign code locally through direnv.
SKIP_DRAFTS=1
SKIP_FORKS=1
SKIP_BOTS=1
SKIP_OWN=1               # review one of your own with `quorum run`

# Reviews
REVIEW_MODEL="gpt-5.6-terra"
REVIEW_EFFORT="medium"
REVIEW_TIMEOUT="45m"     # per reviewer pass, not per run. 0 disables
POST=1                   # 0 writes the comment to disk instead of posting it

# Babysit
FIX_MODEL=""             # empty keeps your codex default
FIX_EFFORT=""
MAX_ITER=12
MAX_CI_FIXES=3
FIX_TIMEOUT="2h"         # per fix step; keep it above your CI runtime
DIVERGENCE_SCAN=0        # analyze the current run after MAX_ITER, then stop
DIVERGENCE_ESCALATE_TO="" # users or org/team slugs to mention, without @
SANDBOXED=0

AGENT_ACTION="review"    # or "babysit"
AUTO_MERGE_AGENT=0       # agent runs, whatever AGENT_ACTION selects
AUTO_MERGE_REVIEW=0      # manual quorum review runs
AUTO_MERGE_BABYSIT=0     # manual quorum babysit runs
AUTO_MERGE_TIMEOUT="2h"  # wait for checks and mergeability; 0 disables timeout
NOTIFY=1
```

Durations accept a bare number of seconds or a value like `30m`, `45m`, `2h`.
Zero disables the timeout it belongs to.

`REVIEW_ARGS` and `MAX_CODEX` are retired. A `REVIEW_ARGS` that contains
`--dry-run` still turns posting off; nothing else in it is read.

### Auto-merge

The three `AUTO_MERGE_*` settings are independent and default to `0`. The agent
uses only `AUTO_MERGE_AGENT`, even when `AGENT_ACTION="babysit"`; `quorum
review` and `quorum run` use `AUTO_MERGE_REVIEW`, and `quorum babysit` uses
`AUTO_MERGE_BABYSIT`.

`AUTO_MERGE_TIMEOUT` defaults to two hours. Set it above the longest protected
check or merge queue wait in the repository. A value of `0` waits until the run
is stopped.

After a posted review with zero Blockers and zero Critical findings, quorum:

1. confirms that GitHub still reports the exact reviewed head;
2. submits an approval tied to that commit, unless the same user already
   approved it; and
3. calls GitHub's merge API with `merge_method=merge` and `sha=SHA`.

Suggestions and Questions do not block. GitHub branch rules and required checks
still apply; optional check failures do not block the merge wait. The merge is
one atomic request for the reviewed SHA, so it fails rather than leaving a new
request that could survive a later push. Target branches that require a merge
queue are rejected before approval. Quorum never
disables an existing auto-merge or merge-queue request because it cannot prove
who created it. Repositories with merge commits disabled are also rejected
before approval. It does not merge an own PR, a moved head, a branch-only run,
`POST=0`, `--dry-run`, or an accepted dispute whose last review still contains
Blockers or Critical findings.

If branch requirements are still pending, Auto-Merge fails instead of queuing a
persistent request. An Auto-Merge failure returns exit code 1. Agent runs record
the review request as handled, so a failure after a successful review does not
spend tokens repeating that review.

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
  when GitHub reports it merged or closed, while it is being reviewed again,
  and two weeks after its last review. The two weeks matter because the state
  file keeps two hundred records and most of them describe pull requests that
  were merged long ago; where GitHub has not been asked, age is the only thing
  the dashboard knows.
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
every run with `POST=0`. The four counts come from the bullets under the
`## Blockers`, `## Critical`, `## Suggestions` and `## Questions` headings.

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
~/Library/LaunchAgents/io.github.quorum.plist  the launchd agent
```

`QUORUM_CONFIG`, `QUORUM_STATE_DIR` and `QUORUM_CLONE_DIR` override the first,
the state directory and the clone directory. The rest follow `XDG_CONFIG_HOME`,
`XDG_STATE_HOME` and `XDG_CACHE_HOME`.

### What gets cleaned up

A successful run deletes its own worktree, which is nearly all of what it took
up. A failed one keeps it so `--resume-run` can pick it up. Run directories are
dropped a week after anything last looked at them, dependency trees after two
weeks. `CACHE_BUDGET_GB` bounds the three cache directories together and every
poll enforces it, so nothing waits for somebody to run `quorum gc`. The managed
clones are outside the budget: one per repository, bounded by how many you
review.

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
ignores your shell profile, so `quorum install` bakes the resolved `PATH` into
the plist. Install a tool somewhere new and you need to run it again.

**gh cannot authenticate from the agent.** `gh` stores its token in the
keychain, which a user agent can only reach while your session is unlocked. Put
`GH_TOKEN` in a file with mode 600 and source it from the config if that is a
problem.

**A PR is stuck after failed reviews.** A failed run leaves the request
unrecorded so the next poll retries, up to `MAX_RETRIES`. After that it is
marked as given up. Retry with `quorum run owner/repo#123`.

**The review was posted against outdated code.** The review starts as soon as
the request appears. If the author keeps pushing, the run refuses to post
against a drifted head and fails, which the retry handles on the next poll.

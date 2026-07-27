# quorum

Several Codex reviewers read your pull request independently. Their findings are
merged into one comment and posted. From there quorum can keep going on its own:
hand the findings to a fix session, wait for CI, review again, until the PR is
clean.

```bash
quorum review 1811     # one review, posted as a comment
quorum babysit 1811    # review, fix, CI, repeat until it is clean
quorum                 # what the agent is doing right now
```

The name is the mechanism: no single reviewer decides anything. Six of them run
against the same diff, the aggregation keeps only what the reviewer outputs
actually support, and a run that cannot produce enough successful reviewers
refuses to post at all.

## What it replaces

quorum is the merge of three tools that grew out of each other:
`pr-codex-review` (the reviewer panel), `babysit` (the review-fix loop) and
`prbot` (the launchd agent). They shared a review core, a cache and a config,
but talked to each other across process boundaries. That is where the fragility
was: the agent used to find a review's output by grepping its stdout for a
`Run dir:` line, and the fix loop worked out which run it had just started by
diffing directory listings of the cache. Both are ordinary return values now.

See [PARITY.md](PARITY.md) for what carried over unchanged, what changed on
purpose, and why.

## Install

```bash
brew install yungweng/tap/quorum
brew install --cask codex     # Homebrew formulas cannot depend on a cask
```

From a checkout, which needs Go 1.25 or newer:

```bash
go build -o ~/.local/bin/quorum .
```

Requirements: `gh` (authenticated), `git`, `codex`. `direnv` is optional and
only needed for projects that have an `.envrc`.

## quorum review

From inside the repository checkout:

```bash
quorum review 1811
quorum review https://github.com/owner/repo/pull/1811
quorum review 1811 -n 8 --model gpt-5.4-mini --effort low
quorum review 1811 --dry-run       # write the comment, do not post it
```

What happens:

1. Read the PR, fetch the base branch and the PR head, create a detached
   worktree under `~/.cache/quorum/reviews/`.
2. `direnv allow`, unless the PR itself changed an `.envrc`.
3. Link in cached dependency trees and enter the environment once, so the
   project's install hook runs at most one time per run instead of once per
   reviewer.
4. Run `codex exec review` several times in parallel.
5. Merge the outputs into one comment, validate its structure, post it.
6. Write `findings.json` and clean up the worktree.

### Safety stops

- **The PR head moved during the review.** The run refuses to post: the findings
  describe code the PR no longer contains. If only the base moved, the run
  continues and the comment carries a note.
- **The PR changed an `.envrc`.** `direnv allow` executes whatever is in that
  file. The run stops unless you pass `--allow-envrc-change` after reading the
  diff yourself.
- **The aggregator produced the wrong shape.** The comment must have exactly
  five sections with findings as bullets. A renamed heading or a finding written
  as prose would count as zero findings and make a PR with real blockers report
  itself clean, so the run retries once and then fails instead of posting.

### Machine-readable findings

Every successful run writes `findings.json` next to the comment:

```json
{
  "schema": 1,
  "pr": 1811,
  "head_sha": "…",
  "reviewers_succeeded": 6,
  "reviewers_requested": 6,
  "blockers": 0,
  "critical": 2,
  "suggestions": 3,
  "questions": 1,
  "posted": true,
  "comment_url": "https://github.com/owner/repo/pull/1811#issuecomment-…"
}
```

### Resume after a stop

A failed run keeps its worktree, so the expensive reviewer passes never have to
run twice:

```bash
quorum review 1811 --resume-run ~/.cache/quorum/reviews/owner-repo-pr-1811-…
```

## quorum babysit

You implement, babysit iterates. It expects the first implementation to be
committed and pushed.

```bash
quorum babysit                  # the PR of the current branch
quorum babysit 1811
quorum babysit 1811 --effort high "Focus on the time-tracking module"
```

The loop: wait for CI, review, decide. Zero Blockers and Critical means done.
Otherwise the review comment goes into a Codex session that checks each finding
for whether it is real or intended, fixes the real ones, commits and pushes. The
pipeline watches CI, posts a comment logging what was fixed, and reviews again.

The next review starts while the pipeline is still waiting for CI, because a
review only needs the pushed head, not green checks.

Only Blockers and Critical keep the loop alive. Suggestions and Questions are
handed to each fix round once, so the loop cannot chase moving targets forever.

All fix rounds share one Codex session, so context carries from the CI fixes
through every review round.

### It runs unattended

The fix sessions use `--dangerously-bypass-approvals-and-sandbox` by default.
They have to: they run tests, use `gh` and `git push`, all with nobody watching,
and a sandboxed `codex exec` would silently skip exactly those commands. Know
what that means. An agent with full file and network access on your machine, for
up to `--fix-timeout` per step. `--sandboxed` opts out, but then your
`~/.codex/config.toml` must allow commands, network and push or every fix round
fails. The review side is unaffected and runs read-only.

The three situations that used to need a human are decided automatically:

- **Product decisions.** The session is told to decide, conservatively, and to
  record notable decisions in the fix-log comment. If it still stops to ask, the
  questions are handed back for it to answer itself; after three rounds of that
  the run gives up rather than looping.
- **Disputed findings.** Review findings can be wrong. A dispute is never
  accepted on first sight: the session has to survive one forced adversarial
  re-check where it actively tries to reproduce each finding. If it fixes
  something after all, the loop continues. If it upholds the dispute, the
  rebuttals are shown in the summary and the run finishes. **The review comment
  on the PR still lists those findings, so read the summary before merging.**
- **Changed `.envrc` files.** The diff is printed and `direnv allow` runs. With
  the sandbox bypassed the session can execute anything anyway, so gating this
  would add no protection.

`--interactive` turns all three into terminal prompts instead. Interactive gates
need a terminal on stdin; without one the run aborts rather than hanging.

### Fix-log comments

Every fix round ends with a comment on the PR describing what was fixed and
which findings were left alone as intended. The session writes the text, in the
language of the PR description; the pipeline posts it, so it appears as an
ordinary comment from you.

### Exit codes

```text
0  CI green and the review clean, or remaining findings disputed and accepted
2  aborted at a gate
3  CI still red after --max-ci-fixes attempts
4  not converged after --max-iter rounds
5  a fix round produced no changes although findings remain
```

### Safety stops

- **Do not push to the PR branch while a run is active.** A review refuses to
  post when the head moves under it, and the pipeline treats that as fatal.
- The pipeline refuses to start on a dirty checkout of the PR branch, or when
  your local branch differs from origin: it reviews the pushed head and would
  otherwise silently ignore your work.
- Fork PRs are refused: the pipeline pushes `refs/heads/<branch>` on origin,
  which for a fork PR is the wrong branch.
- After every push it waits until GitHub reports the new head before reading any
  check result, because `gh pr checks` briefly still answers for the old one and
  a red commit would read as green.
- A fix round that produces no commits while findings remain stops the run.

## The agent

`quorum install` writes a launchd agent that polls for pull requests asking for
your review and handles them on its own.

```text
quorum 1.0.0                             agent loaded, every 5m, last poll 2m ago

RUNNING  1 of 2
  ● project-phoenix #2016      alle Kalender auf den Kit-Picker umstellen
    6m, 4/6 reviewers done

QUEUED  1
  ○ project-phoenix #2014      Jahrgangsstufenwechsel mit reversiblen Abgängen
    waiting for a free slot

RECENT
  ✓ project-phoenix #2002      12h ago    0 blockers, 1 critical, 0 suggestions  comment ↗
  ✗ project-phoenix #1993      yesterday  failed after 2 attempt(s)
```

RUNNING and QUEUED stay on screen when they are empty. A section that
disappears when it has nothing in it cannot be told apart from a section that
does not exist, which is the wrong answer to "is anything being reviewed right
now".

`quorum watch` adds one thing the single-shot dashboard leaves out: it asks
GitHub every 30 seconds how the pull requests on screen ended, and crosses out
the ones that have been merged. That is a whole batch in one GraphQL request,
kept off the redraw path, so a screen that repaints every three seconds neither
waits on the network nor earns a rate limit. Merged is struck through and
labelled `merged`; closed without merging is labelled `closed unmerged` and is
not struck out, because it is not a success. The word matters as much as the
styling: terminals that do not know SGR 9 drop a strikethrough silently.

Polling, not webhooks: a webhook needs a publicly reachable endpoint on your
laptop, and GitHub discards deliveries while it is asleep. Polling catches up by
itself on the next tick. The search costs 12 API requests per hour against a
limit of 30 per minute.

The trigger is the review request, not the code. One request is one review;
pushing more commits does not trigger another one. Re-request the review to get
a fresh one. Being an assignee is not a trigger, only a pending review request.

Requests aimed at a team you belong to are searched separately, because GitHub's
`review-requested:` qualifier does not match those despite the docs.

Set `AGENT_ACTION=babysit` and the agent runs the full fix loop instead of
posting a single review. This was impossible while the two were separate
binaries.

### Commands

```text
quorum                 what is running, queued and finished
quorum watch           the same, redrawn as it changes
quorum run <pr>        hand one PR to the agent right now
quorum logs [n]        follow the log
quorum doctor [--fix]  check the setup and report what to do about it
quorum setup           configure scope, limits and notifications
quorum install         install the launchd agent
quorum uninstall       remove the agent, keep config and state
quorum poll            run one cycle by hand
quorum gc              trim the cache to its budget
quorum config          change any setting, or --path for the file location
```

On Linux everything works except `quorum install`; call `quorum poll` from a
systemd timer or cron.

## Configuration

`quorum config` shows every setting and lets you change it; nothing is reachable
only by opening the file. It is plain `KEY=value` at `~/.config/quorum/config`.

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

# Safety. Fork and bot PRs run foreign code locally through direnv.
SKIP_DRAFTS=1
SKIP_FORKS=1
SKIP_BOTS=1
SKIP_OWN=1               # review one of your own with `quorum run`

# Reviews
REVIEW_MODEL="gpt-5.6-terra"
REVIEW_EFFORT="medium"
REVIEW_TIMEOUT="45m"
POST=1                   # 0 writes the comment to disk instead of posting it

# Babysit
FIX_MODEL=""             # empty keeps your codex default
FIX_EFFORT=""
MAX_ITER=12
MAX_CI_FIXES=3
FIX_TIMEOUT="2h"         # per fix step; keep it above your CI runtime
SANDBOXED=0

AGENT_ACTION="review"    # or "babysit"
NOTIFY=1
```

An existing prbot config and state are picked up automatically, and `quorum
install` unloads the old prbot agent so the two do not race for the same review
requests.

### Keeping the machine usable

What costs a machine here is not the Codex reviewers. They spend their time
waiting on the network: around 1% CPU and 130 MB each. What hurt was the
dependency install, once per reviewer, which the shared dependency cache
removed.

- `MAX_CONCURRENT` is how many reviews run at once, and each runs all
  `REVIEWERS` passes in parallel. A review that trickles through its passes is a
  review you end up waiting for. `quorum doctor` warns when the peak would take
  more than half the machine's memory.
- `NICE` lowers the priority of the whole review process tree.
- `LOAD_LIMIT` is off by default. Turned on, it holds new reviews back while the
  machine is busy; they start on a later poll.

## Cost

One review is several Codex reviewer passes plus an aggregator. `quorum babysit`
multiplies that by the number of rounds and adds a fix session per round. Lower
`REVIEWERS` for cheaper runs, and start with `POST=0` until you trust the
output: your name is on the comment.

Because the agent's trigger is the request rather than the code, a PR costs one
review no matter how many times it is pushed to.

## Shared dependency cache

A project that installs its dependencies from a direnv or devbox hook would pay
for that install once per worktree, and the hook's own "already installed?"
check does not help: all six reviewers enter the environment in the same second,
all six see an empty directory, all six install.

So the environment is entered once before the reviewers start, and whatever the
hook installed is moved to `~/.cache/quorum/deps/<repo>/<project>/<lock-hash>/`.
The next run symlinks it back in, the hook's guard sees a populated directory
and skips the install. Any change to the lock file produces a different hash, so
a stale tree is never reused. Trees no run has needed for 14 days are deleted,
and a cache over its budget gives them up last of all.

Nothing to configure, and `--no-direnv` skips the whole step. Removing a
worktree only removes the symlink, never the shared tree.

## Files

```text
~/.config/quorum/config             configuration
~/.local/state/quorum/state.json    every PR the agent has seen
~/.local/state/quorum/quorum.log    what it did and why it skipped things
~/.local/state/quorum/running/      one marker per review in flight
~/.local/state/quorum/runs/         one log per agent run
~/.cache/quorum/reviews/            review runs and their output
~/.cache/quorum/babysit/            babysit runs, logs and Codex messages
~/.cache/quorum/deps/               shared dependency trees
~/.cache/quorum/repos/              managed clones
```

A successful run deletes its own worktree, which is nearly all of what it took
up; a failed one keeps it so `--resume-run` can pick it up. Run directories are
dropped a week after anything last looked at them, dependency trees after two
weeks. `CACHE_BUDGET_GB` bounds the three cache directories together, and every
poll enforces it, so nothing is waiting on somebody to notice and run
`quorum gc`. Over the budget the worktrees of finished runs go first, then whole
run directories oldest first, then dependency trees, and a run still in flight
is never touched. The managed clones are outside the budget: one per repository,
bounded by how many you review.

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

## License

MIT

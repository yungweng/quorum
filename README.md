# quorum

Several Codex reviewers read your pull request independently. Their findings are
merged into one comment and posted. From there quorum can keep going on its own:
hand the findings to a fix session, wait for CI, review again, until the PR is
clean.

```bash
quorum watch           # follow running, queued and finished work
quorum review          # review the current branch, with or without a PR
quorum babysit 1811    # review, fix, CI, repeat until it is clean
```

The name is the mechanism: no single reviewer decides anything. Six of them run
against the same diff, the aggregation keeps only what the reviewer outputs
actually support, and a run that cannot produce enough successful reviewers
refuses to post at all.

## What it costs

One review is six Codex reviewer passes plus an aggregator. `quorum babysit`
multiplies that by the number of rounds and adds a fix session per round. Lower
`REVIEWERS` for cheaper runs.

**The comment is posted from your GitHub account, under your name.** Start with
`POST=0`, which writes the comment to disk instead, until you trust the output.

Because the agent's trigger is the review request rather than the code, a PR
costs one review no matter how many times it is pushed to.

## Install

```bash
brew install yungweng/tap/quorum
brew install --cask codex     # Homebrew formulas cannot depend on a cask
```

From a checkout, which needs Go 1.25 or newer:

```bash
make install-hooks
make dev
```

This writes `~/.local/bin/quorum` with a version such as
`1.1.0-dev+3d85e2a.dirty`. The commit identifies the checkout; `dirty` means the
build includes uncommitted changes. If `~/.local/bin` precedes Homebrew in
`PATH`, every new terminal uses this build. Check with `which quorum` and
`quorum --version`.

Requires `gh` (authenticated), `git` and `codex`. `direnv` is optional and only
needed for projects that have an `.envrc`.

`make install-hooks` is needed once per clone and after a tracked hook changes.
It copies the reviewed hooks into the clone's untracked Git directory and sets
an absolute `core.hooksPath`. Pre-commit rejects whitespace errors and
unformatted staged Go files, while pre-push runs the full check against the
commits being pushed. Run the same format, race-test, build and lint checks
directly with `make check`.

Claude Code also loads the project hooks from `.claude/`. After it changes Go
or check configuration, its Stop hook runs `make check` and returns failures to
the session. Review new or changed hooks when the client asks.

## quorum review

Run it from inside the repository checkout:

```bash
quorum review                         # current branch; its PR when one exists
quorum review 1811
quorum review https://github.com/owner/repo/pull/1811
quorum review 1811 -n 8 --effort low
quorum review 1811 --dry-run       # write the report, do not post it
```

If the current branch has no open PR, it reviews the pushed branch against the
repository's default branch and writes the report to disk without posting it.
The checkout must be clean and match `origin`; use `--base` to choose another
base branch.

It builds a detached worktree under `~/.cache/quorum/reviews/`, runs `direnv
allow` unless the target itself changed an `.envrc`, links in cached dependency
trees and enters the environment once so the install hook runs one time per run
rather than once per reviewer, then fans out `codex exec review`. The outputs
are merged into one report and checked for structure. PR reviews post it;
branch-only reviews keep it on disk. Each run also writes a machine-readable
`findings.json` beside the report.

A failed run keeps its worktree, so the expensive reviewer passes never have to
run twice: `quorum review 1811 --resume-run <dir>` picks it up with the original
target and base.

A run refuses to post rather than post something misleading: on a moved PR head,
on an `.envrc` the PR itself changed, and on an aggregator answer with the wrong
shape. [The reference](docs/reference.md#safety-stops) says what each means.

## quorum babysit

You implement, babysit iterates. It expects the first implementation to be
committed and pushed.

```bash
quorum babysit                  # current branch; its PR when one exists
quorum babysit 1811
quorum babysit 1811 --effort high "Focus on the time-tracking module"
```

The loop: wait for CI, review, decide. Zero Blockers and Critical means done.
Otherwise the review comment goes into a Codex session that checks each finding
for whether it is real or intended, fixes the real ones, commits and pushes. The
pipeline watches CI, posts a comment logging what was fixed, and reviews again.

If the current branch has no open PR, `babysit` runs the same review-fix rounds
on the clean, pushed branch. It still runs repository checks in each fix step
and confirms every push on `origin`, but it skips PR CI and PR comments because
neither exists yet.

Only Blockers and Critical keep the loop alive. Suggestions and Questions are
handed to each fix round once, so the loop cannot chase moving targets forever.
All fix rounds share one Codex session, so context carries across rounds.

An opt-in divergence scan can explain why a run still has findings at its round
limit:

```text
DIVERGENCE_SCAN=1
DIVERGENCE_ESCALATE_TO="example-user acme/platform"
```

The normal rounds do not change. After the final fix and CI wait, one read-only
analysis compares the current run's review comments, fix responses, disputes
and commits using the configured review model, effort and timeout. It reports
whether the history contains incompatible decisions, only cumulative findings,
or too little evidence to decide, then always stops.
For a one-off run, use `quorum babysit --divergence-scan`. Reports are kept in
the run directory; PR reports mention the author and configured escalation
targets only when a manual decision may be needed.

## Auto-merge

Auto-merge is off by default and enabled separately for each source:

```text
AUTO_MERGE_AGENT=0
AUTO_MERGE_REVIEW=0
AUTO_MERGE_BABYSIT=0
AUTO_MERGE_TIMEOUT="2h"
```

A clean posted PR review has no Blockers or Critical findings; Suggestions and
Questions are allowed. Quorum approves the exact reviewed commit, then asks
GitHub to merge it with a merge commit after the repository's branch rules
pass. It never uses administrator privileges. A moved head, an own PR, a local
report (`POST=0` or `--dry-run`), a branch without a PR, and a target branch
that requires a merge queue are not merged. Repositories with merge commits
disabled are rejected before approval.

The agent setting applies to every agent run, including `AGENT_ACTION=babysit`.
The other two settings apply only to the matching command started in a
terminal. The wait for protected checks and mergeability defaults to two hours;
set `AUTO_MERGE_TIMEOUT=0` to wait until the run is stopped. Change these
settings with `quorum config` or in the config file.

**The fix sessions run with the Codex sandbox bypassed**, which gives them full
file and network access on your machine. `--sandboxed` opts out. Read
[what that means](docs/reference.md#it-runs-unattended) before the first run.

Three situations that used to need a human are decided automatically. Pass
`--interactive` to turn them back into terminal prompts.

- **Product decisions.** The session decides conservatively and records notable
  decisions in the fix-log comment. If it stops to ask anyway, the questions go
  back for it to answer itself; after three rounds of that the run gives up.
- **Disputed findings.** Review findings can be wrong, so a dispute is never
  accepted on first sight: the session must survive one forced re-check where it
  actively tries to reproduce each finding. If the dispute survives, the
  pipeline posts the final rebuttal with a link to the review comment. The
  original review stays unchanged, so the PR conversation preserves both sides.
- **Changed `.envrc` files.** A run stops before loading an `.envrc` changed by
  the target unless you pass `--allow-envrc-change` after reading it. Changes
  made during a fix round are printed before `direnv allow` runs; with the
  sandbox bypassed the session can execute anything anyway.

Every pushed review fix and CI fix ends with a comment on the PR describing what
failed, what changed and which checks ran. The session writes the text, in the
language of the PR description; the pipeline posts it, so it appears as an
ordinary comment from you.

**Do not push to the PR branch while a run is active.** The pipeline also
refuses dirty or diverged checkouts and fork PRs;
[the reference](docs/reference.md#safety-stops) says why.

## The agent

`quorum install` writes a launchd agent that polls for pull requests asking for
your review and handles them on its own.

```text
quorum 1.1.0                                        agent loaded, every 5m, last poll 2m ago

  1/2 review   ·   1 fix   ·   1 queued   ·   3.4 load                                     ⠋
────────────────────────────────────────────────────────────────────────────────────────────

OPEN  2
  ● payments #98                accept crumb refunds in the same currency
      reviewed 21:02 · 2B 1C 3S  comment ↗
  ● toaster-api #2002           read the crumb tray size from the config
      reviewed 19:42 · nothing found  comment ↗

ACTIVE  1 / 2
  ● toaster-api #2016           stop emailing every user at 3am about crumbs
      review · agent · 6m · 4/6 reviewers done
  ◆ payments #103               harden artifact health screening
      review · manual · 2m · 0/6 reviewers done
  ● toaster-api #2018           make the crumb tray endpoint idempotent
      1h04m   round 3/12   CI ✗ fix 2/3   review ✓   CI fix 2 ● 4m   5 commits   log ↗
  ○ toaster-api #2014           allow browning levels above 11
      queued · waiting for a free slot

HISTORY
  ✓  21:02   payments #98       @robin     2B 1C 3S       comment ↗
  ✓  19:42   toaster-api #2002  @sam       2 runs         nothing found  comment ↗  merged
  ✗  29 Jul  toaster-api #1993  @robin     failed         reviewer-2 timed out after 45m

every repo that asks you · 2 at a time, 6 reviewers each · auto-merge off · 5.0 GB cache
```

The status bar under the version answers "what is this machine doing" before
anything else has to be read.

OPEN lists pull requests quorum has reviewed that are still open, newest first,
with what the review found. The bullet is red for blockers, yellow for critical
findings and green for neither. A PR waiting on GitHub's Auto-Merge or merge
queue says `auto-merge queued`; the others still need a person. A pull request
leaves the section when it is merged or closed, when it is being reviewed again
(ACTIVE has it then), and two weeks after its last review.

ACTIVE is everything in flight: reviews the agent started, reviews you started
in a terminal, fix loops, and what is waiting for a slot. The symbol and the
label under each line say which is which. Its count covers agent slots only, so
a review or a fix loop you ran yourself does not spend one. On an idle machine
the whole section is one line saying so.

HISTORY is one line per pull request, newest first, however its runs were
started. `toaster-api #2002` says `2 runs`, a review and then a fix loop: the
log keeps every run, and the line says how many there were and how many of them
failed rather than spending a row on each. `HISTORY=20` in the config sets how
many are listed. The log behind it is described in
[the reference](docs/reference.md#the-history-log).

Every section draws the same columns at the same widths, measured from what is
actually on screen. On a narrow terminal the columns give way in a fixed order,
whole rather than shortened to a stub: the author first, then the explanation
behind a result, and the repository and number last.

`quorum watch` redraws the same screen as it changes and marks pull requests
that have since been merged or closed.

The trigger is the review request, not the code. One request is one review, so
pushing more commits does not trigger another one; re-request the review to get
a fresh one. Being an assignee is not a trigger. Requests aimed at a team you
belong to are searched separately, because GitHub's `review-requested:`
qualifier does not match those despite the docs.

Set `AGENT_ACTION=babysit` and the agent runs the full fix loop instead of
posting a single review.

### Commands

```text
quorum                 show the command overview
quorum watch           follow running, queued and finished work
quorum status          show one dashboard snapshot
quorum run <pr>        hand one PR to the agent right now
quorum logs [-n N]     follow the log, --no-follow to just print the tail
quorum doctor [--fix]  check the setup and report what to do about it
quorum setup           configure scope, limits and notifications
quorum install         install the launchd agent
quorum uninstall       remove the agent, keep config and state
quorum poll            run one cycle by hand
quorum gc [--dry-run]  trim the cache to its budget
quorum config          change any setting, or --path for the file location
quorum version         print the version
```

On Linux everything works except `quorum install`; call `quorum poll` from a
systemd timer or cron.

## Configuration and reference

`quorum config` shows every setting and lets you change it; nothing is reachable
only by opening the file. It is plain `KEY=value` at `~/.config/quorum/config`.

[docs/reference.md](docs/reference.md) has the full reference: every config key
with its default, every command flag, the exit codes, the files quorum reads and
writes, and troubleshooting.

An existing prbot config and state are picked up automatically, and `quorum
install` unloads the old prbot agent so the two do not race.

## License

MIT

# quorum

Several Codex reviewers read your pull request independently. Their findings are
merged into one comment and posted. From there quorum can keep going on its own:
hand the findings to a fix session, wait for CI, review again, until the PR is
clean.

```bash
quorum watch           # follow running, queued and finished work
quorum review          # review the PR of the current branch
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
make dev
```

This writes `~/.local/bin/quorum` with a version such as
`1.1.0-dev+3d85e2a.dirty`. The commit identifies the checkout; `dirty` means the
build includes uncommitted changes. If `~/.local/bin` precedes Homebrew in
`PATH`, every new terminal uses this build. Check with `which quorum` and
`quorum --version`.

Requires `gh` (authenticated), `git` and `codex`. `direnv` is optional and only
needed for projects that have an `.envrc`.

## quorum review

Run it from inside the repository checkout:

```bash
quorum review                         # the PR of the current branch
quorum review 1811
quorum review https://github.com/owner/repo/pull/1811
quorum review 1811 -n 8 --effort low
quorum review 1811 --dry-run       # write the comment, do not post it
```

It builds a detached worktree under `~/.cache/quorum/reviews/`, runs `direnv
allow` unless the PR itself changed an `.envrc`, links in cached dependency
trees and enters the environment once so the install hook runs one time per run
rather than once per reviewer, then fans out `codex exec review`. The outputs
are merged into one comment, checked for structure, and posted. Each run also
writes a machine-readable `findings.json` beside the comment.

A failed run keeps its worktree, so the expensive reviewer passes never have to
run twice: `quorum review 1811 --resume-run <dir>` picks it up.

A run refuses to post rather than post something misleading: on a moved PR head,
on an `.envrc` the PR itself changed, and on an aggregator answer with the wrong
shape. [The reference](docs/reference.md#safety-stops) says what each means.

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

Only Blockers and Critical keep the loop alive. Suggestions and Questions are
handed to each fix round once, so the loop cannot chase moving targets forever.
All fix rounds share one Codex session, so context carries across rounds.

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
  actively tries to reproduce each finding. **A dispute it upholds still appears
  on the PR as a review finding, so read the run summary before merging.**
- **Changed `.envrc` files.** The diff is printed and `direnv allow` runs. With
  the sandbox bypassed the session can execute anything anyway.

Every fix round ends with a comment on the PR describing what was fixed and what
was left alone as intended. The session writes the text, in the language of the
PR description; the pipeline posts it, so it appears as an ordinary comment from
you.

**Do not push to the PR branch while a run is active.** The pipeline also
refuses dirty or diverged checkouts and fork PRs;
[the reference](docs/reference.md#safety-stops) says why.

## The agent

`quorum install` writes a launchd agent that polls for pull requests asking for
your review and handles them on its own.

```text
quorum 1.0.3                             agent loaded, every 5m, last poll 2m ago

REVIEWING  1 of 2
  ● toaster-api #2016          stop emailing every user at 3am about crumbs
    agent · 6m, 4/6 reviewers done
  ◆ payments #103             harden artifact health screening
    manual · 2m, 0/6 reviewers done

BABYSITTING  2
  ● toaster-api #2018          make the crumb tray endpoint idempotent
    1h04m   round 3/12   CI ✗ fix 2/3   review ✓   CI fix 2 ● 4m   5 commits   log ↗
  ● toaster-api #2019          unflake the sourdough integration test
    22m   round 1/12   CI ✓   review 0B 2C   fixing ● 18m   1 commit   log ↗

QUEUED  1
  ○ toaster-api #2014          allow browning levels above 11
    waiting for a free slot

RECENT
  ✓ toaster-api #2002          12h ago    0 blockers, 1 critical, 0 suggestions  comment ↗
  ✗ toaster-api #1993          yesterday  failed after 2 attempt(s)
```

REVIEWING lists both agent reviews and `quorum review` commands running in a
terminal. The label and symbol show which started each run. Its count covers
agent slots only; a manual review does not spend one.

BABYSITTING lists every fix loop in flight, whether the agent started it or you
ran `quorum babysit` in a terminal. A fix loop can sit in one place for an hour,
so the line answers "is it getting anywhere" rather than "is it running". It
carries no slot count: a loop the agent started already spends its slot under
REVIEWING, and one you ran yourself spends none.

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

# prbot

Watches GitHub for pull requests that request your review and runs
[`pr-codex-review`](https://github.com/yungweng/pr-codex-review) on each one
automatically. Runs entirely on your own machine, no server and no webhook
endpoint required.

The loop it replaces:

1. Someone assigns you as a reviewer.
2. You notice at some point.
3. You clone or fetch the repo, run `pr-codex-review <number>`, wait.

With prbot installed, steps 2 and 3 happen on their own and you get a
notification when the review comment is posted.

## Requirements

- macOS for the bundled launchd agent. On Linux everything works except
  `prbot install`; call `prbot poll` from a systemd timer or cron instead.
- `gh`, authenticated (`gh auth login`)
- `jq`, `git`
- `pr-codex-review` on your `PATH`, which in turn needs `codex` and `direnv`

## Install

Via Homebrew:

```bash
brew tap yungweng/tap
brew install prbot
```

That pulls in `gh`, `jq`, and `pr-codex-review` automatically. The Codex CLI
has to be installed separately, because Homebrew ships it as a cask and
formulas cannot depend on casks:

```bash
brew install --cask codex
```

Or from a checkout:

```bash
git clone https://github.com/yungweng/prbot.git
ln -sf "$PWD/prbot/bin/prbot" ~/.local/bin/prbot
```

Then set it running:

```bash
gh auth login     # if you have not already
prbot install
```

`prbot install` writes a launchd agent that runs `prbot poll` every five
minutes, creates a default config, and starts the agent immediately.

Check that it is alive:

```bash
prbot status
```

## How it works

Polling, not webhooks. A webhook would need a publicly reachable endpoint on
your laptop, and GitHub discards deliveries while that endpoint is asleep.
Polling recovers by itself: the laptop wakes up, the next tick compares against
the stored state and catches up. The search costs 12 API requests per hour
against a limit of 30 per minute.

Each poll cycle:

1. `gh search prs --review-requested=@me --state=open` across every org and
   repo you have access to, narrowed by `ORGS` / `REPOS` if you set them.
   GitHub's `review-requested:` qualifier only matches requests aimed at you
   personally, so prbot additionally searches `team-review-requested:` for
   every team you belong to and merges the results. Being an *assignee* on a
   PR is not a trigger, only a pending review request is.
2. Reads each PR's timeline and takes the timestamp of the most recent
   `review_requested` event aimed at you or one of your teams. That timestamp
   is the trigger. If prbot already handled it, the PR is skipped; otherwise
   the review starts right away.
3. Fetches the head SHA, draft flag, fork flag, and author for each candidate.
4. Skips drafts, fork PRs, and bot authors by default.
5. Clones or fetches the repo under `~/.cache/prbot/repos/` and runs
   `pr-codex-review` there.
6. Reads `findings.json`, records the result, sends a notification.

One request means one review. Pushing more commits afterwards does **not**
trigger another one, because nothing new was requested. To get a fresh review
of a newer state, remove the review request and add it again, or use the
re-request button once a review has been submitted. That writes a new
`review_requested` event, which prbot treats as a new trigger.

Your own PRs are skipped by default (`SKIP_OWN=1`): you already watch those,
and reviewing them just burns Codex runs. Trigger one on purpose with
`prbot run`.

Reviews run in parallel as detached background processes, up to
`MAX_CONCURRENT` at a time (default 3). A per-PR marker under the state
directory prevents the same PR from being reviewed twice at once, and polls
keep firing while reviews run, so a long review no longer blocks the queue.

## Commands

```text
prbot install         install the launchd agent
prbot uninstall       remove the agent, keep config and state
prbot poll            run one cycle by hand
prbot run <pr>        review one PR now: URL, owner/repo#number, or number
prbot status          agent state, recent reviews, log tail
prbot logs [n]        follow the log
prbot config          print the config path, creating a default if missing
```

## Configuration

Config lives at `~/.config/prbot/config` (or `$XDG_CONFIG_HOME/prbot/config`)
and is plain shell syntax.

```bash
# Scope. Empty means every review request assigned to you, anywhere.
ORGS=""                  # e.g. "acme-inc myorg"
REPOS=""                 # e.g. "acme-inc/api acme-inc/web"
EXCLUDE_REPOS=""         # e.g. "acme-inc/legacy"

# Requests aimed at a team you belong to are searched separately.
INCLUDE_TEAMS=1
TEAMS=""                 # empty discovers your teams; or "acme-inc/backend"

# A review starts as soon as a new request for you appears, so there is no
# pacing to configure. These only bound failure and concurrency.
MAX_RETRIES=3
MAX_CONCURRENT=3         # reviews running at once; each spawns several Codex runs
POLL_INTERVAL=300        # re-run `prbot install` after changing this

# Safety
SKIP_DRAFTS=1
SKIP_FORKS=1
SKIP_BOTS=1
SKIP_OWN=1               # skip PRs you authored; `prbot run` reviews them anyway

# Passed straight to pr-codex-review
REVIEW_ARGS=""

NOTIFY=1
```

### Reviewing without posting

Set `REVIEW_ARGS="--dry-run"` to have every automatic review write its comment
to disk instead of posting it. `prbot status` shows the run directory, and you
can post later with `pr-codex-review <number> --resume-run <dir>`. This is the
cautious way to start until you trust the output.

## Cost and safety

Read this before turning it on.

- **Cost.** One review is several Codex reviewer passes plus an aggregator.
  Because the trigger is the request rather than the code, a PR costs one
  review no matter how many times it is pushed to. Lower `-n` through
  `REVIEW_ARGS` if you want cheaper runs, for example `REVIEW_ARGS="-n 3"`.
- **Automatic posting.** By default the comment goes to the PR without you
  reading it first. Your name is on it. Use `REVIEW_ARGS="--dry-run"` while you
  build trust.
- **Code execution.** `pr-codex-review` runs `direnv allow` and `codex exec`
  inside a worktree of the PR's code. Unattended, that means foreign code runs
  on your machine without you watching. `SKIP_FORKS=1` and `SKIP_BOTS=1` exist
  for exactly this reason, and turning them off is a deliberate decision.
  prbot never passes `--allow-envrc-change`.

## Files

```text
~/.config/prbot/config              configuration
~/.local/state/prbot/state.json     reviewed head SHAs, results, daily counters
~/.local/state/prbot/prbot.log      what the agent did and why it skipped things
~/.local/state/prbot/runs/          stdout of each pr-codex-review invocation
~/.cache/prbot/repos/               managed clones
~/.cache/pr-codex-review/           the review outputs themselves
```

## Troubleshooting

**Nothing happens.** `prbot status` shows whether the agent is loaded. Then
read `~/.local/state/prbot/prbot.log`; every skip is logged with its reason.

**Works in the terminal, not from launchd.** Almost always a `PATH` problem.
launchd ignores your shell profile, so `prbot install` bakes the resolved
`PATH` into the plist. If you install a tool afterwards into a new location,
run `prbot install` again to refresh it.

**gh cannot authenticate from the agent.** `gh` stores its token in the
keychain, which a user agent can only reach while your session is unlocked. If
that is a problem, put `GH_TOKEN` into a file with mode 600 and source it from
the config.

**A PR is stuck after failed reviews.** A failed run leaves the request
unrecorded so the next poll retries it, up to `MAX_RETRIES` attempts. After
that prbot marks the request as given up and moves on. Retry manually with
`prbot run owner/repo#123`.

**The review was posted against outdated code.** The review starts as soon as
the request appears. If the author keeps pushing, `pr-codex-review` refuses to
post against a drifted head and the run fails, which the retry then handles on
the next poll.

## License

MIT

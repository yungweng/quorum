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
- `git`
- `pr-codex-review` on your `PATH`, which in turn needs `codex` and `direnv`

## Install

Via Homebrew:

```bash
brew tap yungweng/tap
brew install prbot
```

That pulls in `gh` and `pr-codex-review` automatically. The Codex CLI
has to be installed separately, because Homebrew ships it as a cask and
formulas cannot depend on casks:

```bash
brew install --cask codex
```

Or from a checkout, which needs Go 1.25 or newer:

```bash
git clone https://github.com/yungweng/prbot.git
cd prbot && go build -o ~/.local/bin/prbot .
```

Then set it running:

```bash
gh auth login     # if you have not already
prbot install
```

`prbot install` writes a launchd agent that runs `prbot poll` every five
minutes, creates a default config, and starts the agent immediately.

Then `prbot setup` walks through scope, how much of the machine reviews may
take, and notifications, and `prbot` on its own shows what is happening:

```text
prbot 0.5.0                              agent loaded, every 5m, last poll 2m ago

RUNNING  1 of 2
  ● project-phoenix #2016      alle Kalender auf den Kit-Picker umstellen
    6m, 4/6 reviewers done

QUEUED  1
  ○ project-phoenix #2014      Jahrgangsstufenwechsel mit reversiblen Abgängen
    waiting for a free slot

RECENT
  ✓ project-phoenix #2002      12h ago    0 blockers, 1 critical, 0 suggestions  comment ↗
  ✗ project-phoenix #1993      yesterday  failed after 2 attempt(s)

SYSTEM
  scope      every repo that asks you
  budget     6 reviews at a time, 6 reviewers each, so up to 36 Codex processes
  load       3.0
  cache      1.9 GB of 5.0 GB
```

The repository and number are links: in a terminal that supports them, clicking
opens the pull request, and `comment ↗` opens the review that was posted.

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
prbot                 what is running, queued and finished
prbot watch           the same, redrawn as it changes
prbot run <pr>        review one PR now: URL, owner/repo#number, or number
prbot logs [n]        follow the log
prbot doctor [--fix]  check the setup and report what to do about it
prbot setup           configure scope, limits and notifications
prbot install         install the launchd agent
prbot uninstall       remove the agent, keep config and state
prbot poll            run one cycle by hand
prbot gc              trim the review cache to its budget
prbot config          change any setting, or --path for the file location
```

## Configuration

`prbot config` shows every setting with its current value and lets you change
any of them; there is nothing that can only be reached by opening the file.
`prbot setup` is the shorter guided version for a first run. Both write
`~/.config/prbot/config` (or `$XDG_CONFIG_HOME/prbot/config`), plain
`KEY=value` lines, the same file the shell version used.

```text
prbot config

  scope            every repository that asks you
  never review     none
  team requests    picked up, teams discovered automatically
  reviews at once  6
  reviewers each   6, all in parallel, so up to 36 Codex processes at peak
  poll interval    every 2m
  attempts         3 before prbot gives up on a request
  priority         nice 10, reviews give way to your own work
  load limit       off, reviews start whenever they are requested
  cache budget     5 GB, trimmed by prbot gc
  ...

  ↑↓ to move, enter to change, s to save, q to leave
```

`prbot config --path` prints just the file location, for scripts.

```bash
# Scope. Empty means every review request assigned to you, anywhere.
ORGS=""                  # e.g. "acme-inc myorg"
REPOS=""                 # e.g. "acme-inc/api acme-inc/web"
EXCLUDE_REPOS=""         # e.g. "acme-inc/legacy"

# Requests aimed at a team you belong to are searched separately.
INCLUDE_TEAMS=1
TEAMS=""                 # empty discovers your teams; or "acme-inc/backend"

# How much runs at once. Every review runs all REVIEWERS passes in parallel,
# so at peak there are MAX_CONCURRENT x REVIEWERS Codex processes.
MAX_CONCURRENT=6         # reviews running at once
REVIEWERS=6              # reviewer passes per review
NICE=10                  # scheduling priority for reviews, 0 disables
LOAD_LIMIT=0             # hold reviews back above this 1-minute load, 0 disables
CACHE_BUDGET_GB=5        # review cache is trimmed to this size, 0 disables

MAX_RETRIES=3
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

### Keeping the machine usable

What actually costs a machine here is not the Codex reviewers. They spend their
time waiting on the network: measured at around 1% CPU and 130 MB each. What
hurt was the dependency install, once per reviewer, which is what the shared
dependency cache in `pr-codex-review` removed.

So the limits are about memory and about staying out of your way, not about
rationing parallelism:

- `MAX_CONCURRENT` is how many reviews run at once, and every one of them runs
  all `REVIEWERS` passes in parallel. A review that trickles through its passes
  is a review you end up waiting for, which defeats the point. `prbot` shows
  the peak under `budget`, and `prbot doctor` warns when it would take more
  than half of the machine's memory.
- `NICE` lowers the priority of the whole review process tree, so reviews give
  way to whatever you are doing yourself.
- `LOAD_LIMIT` is off by default. Turned on, it holds new reviews back while
  the machine is already busy; they wait in the queue and start on a later
  poll, shown as `held back: system busy`.

`prbot gc` keeps `~/.cache/pr-codex-review` inside `CACHE_BUDGET_GB`. It drops
the worktrees of finished runs first, since they hold nearly all of the space
and the review output stays readable without them.

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
~/.local/state/prbot/state.json     every PR prbot has seen, and what became of it
~/.local/state/prbot/prbot.log      what the agent did and why it skipped things
~/.local/state/prbot/running/       one marker per review in flight
~/.local/state/prbot/runs/          stdout of each pr-codex-review invocation
~/.cache/prbot/repos/               managed clones
~/.cache/pr-codex-review/           the review outputs themselves
```

## Troubleshooting

**Nothing happens.** `prbot doctor` checks the tools, the GitHub login, the
agent and whether it has actually completed a poll recently, and prints the
command to fix whatever it found. Every skip also carries its reason in
`prbot` output and in `~/.local/state/prbot/prbot.log`.

**A GitHub call failed.** Transient failures (TLS timeouts, a token refresh
racing a request into an HTTP 401, search rejecting a query it accepted a
minute earlier) are retried with a backoff and reported as retries. They are
never recorded as a decision not to review, so a hiccup cannot make a pull
request silently disappear from the queue.

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

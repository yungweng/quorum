# CLAUDE.md

Guidance for coding agents working in this repository.

## What this is

One Go binary that reviews pull requests with a panel of Codex reviewers
(`quorum review`), drives a PR to green through review-fix rounds
(`quorum babysit`), and does either of those unattended from a launchd agent
(`quorum`, `quorum install`).

It is a port of three separate tools: `pr-codex-review` 1.6.0, `babysit` 0.6.2
(both Bash) and `prbot` 0.5.1 (Go). The safety stops that carried over are the
constraints listed below, and each has a test. [docs/parity.md](docs/parity.md)
is the migration ledger from that port: outdated, kept for reference only.

User-facing documentation is [README.md](README.md) and
[docs/reference.md](docs/reference.md).

## Layout

```text
main.go              the entry point: var Version, and one call into internal/cli

internal/cli/        the command line
  app.go             the app struct, command dispatch, tool resolution, PATH widening
  flags.go           the argument parser (options and positionals may interleave)
  repo.go            resolving a PR argument and the repository it belongs to
  review_cmd.go      `quorum review` + its terminal reporter
  babysit_cmd.go     `quorum babysit` + its terminal reporter
  poll.go            the agent's poll cycle, and orphan recovery
  status.go          the dashboard, and the merge-state tracker `watch` runs
  run.go             run, logs, watch
  gc.go              the cache collector and everything that measures the cache
  doctor.go setup.go configui.go agent.go

internal/review/     the reviewer panel: worktree, fan-out, aggregation, findings
internal/loop/       the review-fix pipeline: CI, fix sessions, gates
internal/deps/       shared dependency trees, keyed by lock file hash
internal/codex/      codex exec wrapper, flag construction, session recovery
internal/proc/       run with a timeout, kill the whole process group
internal/envexec/    run a command inside the worktree, through direnv
internal/gh/         the GitHub CLI, with one retry policy
internal/git/        git, always against an explicit directory
internal/config/ internal/state/ internal/paths/ internal/ui/ internal/logbook/
internal/runner/     what the agent does per pull request
```

The dependency direction is one way: `cli` uses everything, `runner` uses `loop`
and `review`, `loop` uses `review`, and `review` uses `codex`, `deps`,
`envexec`, `gh`, `git`. Keep it that way; a cycle here usually means a piece of
policy ended up in the wrong layer.

`Version` lives in `main.go` rather than in `internal/cli` and is passed to
`cli.Run`. Both the Homebrew formula (`-ldflags "-X main.Version=..."`) and the
release job in `ci.yml` (which greps the literal `var Version = "X.Y.Z"` line
out of `main.go`) depend on that. Moving it breaks release and version
reporting without breaking the build.

## Constraints

- **Safety stops are deliberate.** Refusing to post on head drift, refusing
  automatic `direnv allow` on an `.envrc` the PR changed, refusing a fenced or
  structurally wrong aggregator answer, refusing dirty or diverged checkouts and
  fork PRs, stopping a fix round that produced nothing. Do not weaken one to
  make a run pass. Each has a test.
- **The five-section contract is load-bearing.** `findings.json` is produced by
  counting bullets under `## Blockers`, `## Critical`, `## Suggestions` and
  `## Questions`. A renamed heading or a finding written as prose counts as
  zero, which turns a PR with real blockers into a clean one. That is why
  `ValidateComment` fails the run instead of posting.
- **Only Blockers and Critical keep the loop alive.** Suggestions and Questions
  go to each fix round once. Making either of them blocking prevents
  convergence.
- **Fix sessions run with the sandbox bypassed** (`--sandboxed` opts out). They
  push and run tests unattended, which a sandboxed `codex exec` would silently
  skip. Reviews never get the bypass and run read-only.
- **The pipeline posts fix-log comments, never Codex.** The session writes the
  text under a `PR COMMENT:` marker; the pipeline posts it via `gh`. No comment
  may mention AI or automation.
- **Session recovery matches on the worktree path**, which is unique per run.
  That uniqueness is the entire reason resuming cannot hijack a concurrent Codex
  session started elsewhere. If you change the run directory layout, keep it.
- **Commands run in their own process group** so a timeout reaches every
  descendant. Codex spawns MCP servers and toolchains below itself. Do not
  replace `internal/proc` with a plain `exec.CommandContext`; its kill only
  reaches the direct child.
- **The dashboard may only read small files.** It repaints every three seconds,
  so what it reads per frame has to stay cheap. Fix loops report themselves
  through `progress.json`, which is rewritten whole on every phase change;
  reviewers report through `output/events.log`. Neither the Codex logs nor the
  run's message files are ever parsed for progress: `fix-round-1.log` passes a
  megabyte within minutes. GitHub is asked outside the frame as well: `watch`
  polls merge states in a goroutine beside the redraw, and `status` asks once
  before it draws. A network call inside `dashboard` would make the screen wait
  on it every three seconds.
- **`gh pr checks` is not trustworthy right after a push.** It briefly still
  answers for the previous head, so a red commit reads as green. Never read a
  check result before GitHub reports the pushed sha as the PR head.
- **Help text, option parsing and `docs/reference.md` describe the same flags.**
  When you touch one, update all three. The README shows only the flags worth an
  example; the full table lives in the reference.

## Local development

Run the working tree directly while changing code:

```bash
go run . [command] [args...]
```

To make the current checkout the `quorum` used by the shell, rebuild the local
binary:

```bash
go build -o ~/.local/bin/quorum .
```

On the maintainer's machine, `~/.local/bin` precedes `/opt/homebrew/bin`, so
this local build wins over the Homebrew installation. `which quorum` should
print `/Users/yonnock/.local/bin/quorum`; the Homebrew binary remains available
at `/opt/homebrew/bin/quorum`.

## Checks before committing

Install trusted copies of the tracked Git hooks once per clone and after a hook
changes:

```bash
make install-hooks
```

Run the shared format, race-test, build and lint checks with:

```bash
make check
go run . --help && go run . review --help && go run . babysit --help
```

The project-local Claude Code Stop hook runs `make check` after a session
changes Go or check configuration. Its state lives under `.git` and must not
be moved into the working tree.

## Test data

Test fixtures must be fictional. Never copy real usernames, organisations,
repositories, branches, pull request numbers, titles, URLs, IDs or payloads
into tests. Use obvious placeholders such as `acme/api`, `example-user` and PR
`42`. Go module import paths are code references, not fixtures.

## Testing what has no test

The end-to-end path spends real Codex tokens, so it is not in CI. When you
change `internal/review` or `internal/loop`, run this against a PR you do not
mind, in order:

1. `quorum review <pr> --dry-run -n 2` for the reviewer fan-out, the dependency
   cache, aggregation and validation, without posting.
2. `quorum review <pr> -n 2` for posting and `findings.json`.
3. `quorum babysit <pr> --max-iter 1` for the fix session, session recovery, the
   push barrier and the fix-log comment.

## Releases

Distribution is the Homebrew tap `yungweng/homebrew-tap`. The formula there is
updated automatically; never edit it by hand for a version bump.

Releasing is one edit: set `Version` in `main.go` and merge it to `main`. Do not
tag by hand. The `release` job in `ci.yml` reads that constant and creates the
tag and release only when the current commit changed it from its first parent
and no `vX.Y.Z` release exists yet. A merge that leaves `Version` alone releases
nothing, so ordinary changes cannot retry a failed release from the wrong
commit; rerun the version-changing workflow instead.

Patch for bugfixes, minor for anything that adds or changes a flag.

The formula bump then runs as the last step of that release job, which is
deliberate and easy to get wrong if you touch it: GitHub does not start a
workflow from an event raised with the default `GITHUB_TOKEN`, so a release the
CI cut for itself never arrives as a `release: published` event. Releases
published by hand still enter through `update-homebrew-tap.yml`. Both paths use
the same action and the same non-cancelling concurrency queue, so release
creation and the formula push are one critical section. The action's formula
push is also an optimistic compare-and-swap: it fetches the current tap branch,
refuses to replace a newer formula tag with an older one, and retries from the
new head when another release pushes first.

If its version is still the newest, rerunning the version-changing workflow
carries an existing tag through to the formula step, so a failed update can be
retried without creating the release again. The retry first verifies that the
existing tag resolves to the same commit; it fails rather than updating the
formula from a same-version release elsewhere. The same check runs before
release creation when a tag exists without a release, because GitHub would
otherwise ignore `--target` and publish from that tag.

# CLAUDE.md

Guidance for coding agents working in this repository.

## What this is

One Go binary that reviews pull requests with a panel of Codex reviewers
(`quorum review`), drives a PR to green through review-fix rounds
(`quorum babysit`), and does either of those unattended from a launchd agent
(`quorum`, `quorum install`).

It is a port of three separate tools: `pr-codex-review` 1.6.0, `babysit` 0.6.2
(both Bash) and `prbot` 0.5.1 (Go). Read [PARITY.md](PARITY.md) before changing
behaviour: it lists every safety stop that carried over, with a reference to the
line in the original that motivated it, and every deliberate deviation.

## Layout

```text
main.go              command dispatch, tool resolution, PATH widening
flags.go             the argument parser (options and positionals may interleave)
review_cmd.go        `quorum review` + its terminal reporter
babysit_cmd.go       `quorum babysit` + its terminal reporter
poll.go              the agent's poll cycle
status.go            the dashboard
commands.go          run, logs, gc
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

The dependency direction is one way: `runner` uses `loop` and `review`, `loop`
uses `review`, and `review` uses `codex`, `deps`, `envexec`, `gh`, `git`. Keep
it that way; a cycle here usually means a piece of policy ended up in the wrong
layer.

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
- **`gh pr checks` is not trustworthy right after a push.** It briefly still
  answers for the previous head, so a red commit reads as green. Never read a
  check result before GitHub reports the pushed sha as the PR head.
- **Help text, option parsing and the README describe the same flags.** When you
  touch one, update all three.

## Checks before committing

```bash
gofmt -l .
go test -race ./...
golangci-lint run ./...
go run . --help && go run . review --help && go run . babysit --help
```

## Testing what has no test

The end-to-end path spends real Codex tokens, so it is not in CI. When you
change `internal/review` or `internal/loop`, run the ladder in the last section
of PARITY.md against a PR you do not mind, starting with
`quorum review <pr> --dry-run -n 2`.

## Releases

Distribution is the Homebrew tap `yungweng/homebrew-tap`. The formula there is
updated automatically; never edit it by hand for a version bump.

1. Set `Version` in `main.go`.
2. Commit and push to `main`.
3. `git tag vX.Y.Z && git push origin vX.Y.Z`
4. `gh release create vX.Y.Z --title "vX.Y.Z" --notes "..."`

Publishing the release triggers `update-homebrew-tap.yml`, which recomputes the
tarball SHA and pushes the bumped formula using the `TAP_DEPLOY_KEY` secret.

Patch for bugfixes, minor for anything that adds or changes a flag.

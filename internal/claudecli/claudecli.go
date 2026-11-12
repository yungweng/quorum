// Package claudecli drives the Claude Code CLI as an engine, mirroring what
// internal/codex does for Codex: flag construction, the review-side passes
// and the resumable fix session. The differences are shape, not policy:
// Claude has no built-in review mode, so the reviewer prompt is quorum's own;
// it prints its result as a JSON envelope on stdout instead of writing a file;
// and it hands the session id back directly instead of leaving it to a
// rollout scan.
package claudecli

import (
	"context"
	"io"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/headless"
)

// Options are the model settings for one Claude invocation.
type Options struct {
	Bin   string // the claude binary to run
	Model string // empty leaves Claude Code's own default

	// Effort is the reasoning effort, one of the levels the engine table
	// lists for claude; empty leaves Claude Code's own default. Whether a
	// given model honours it is the CLI's business, not quorum's.
	Effort string

	// Bypass adds --dangerously-skip-permissions. The fix sessions need it:
	// they run tests, use gh and push, unattended. The review side never sets
	// it and runs with a fixed read-only tool set instead.
	Bypass bool
}

func (o Options) flags() []string {
	f := []string{"-p", "--output-format", "json"}
	if o.Model != "" {
		f = append(f, "--model", o.Model)
	}
	if o.Effort != "" {
		f = append(f, "--effort", o.Effort)
	}
	if o.Bypass {
		f = append(f, "--dangerously-skip-permissions")
	}
	return f
}

// readOnlyFlags is the review-side posture, hardcoded the way the Codex
// engine hardcodes --ephemeral: only the read tools exist for the model, no
// MCP servers load, anything that would prompt for permission is auto-denied,
// and nothing resumable is left behind.
func readOnlyFlags() []string {
	return []string{
		"--tools", "Read,Grep,Glob",
		"--strict-mcp-config",
		"--permission-mode", "dontAsk",
		"--no-session-persistence",
	}
}

// resultEnvelope is claude --output-format json's stdout schema; the fields
// quorum needs from it, everything else is ignored.
type resultEnvelope struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

// Answer, Session and Failure read the envelope for headless.CLI.
func (e resultEnvelope) Answer() string  { return e.Result }
func (e resultEnvelope) Session() string { return e.SessionID }
func (e resultEnvelope) Failure([]byte) (string, bool) {
	return e.Result, e.IsError
}

// cli is the headless runner with these settings. The prompt goes in on
// stdin.
func (o Options) cli() headless.CLI[resultEnvelope] {
	return headless.CLI[resultEnvelope]{
		Name: "claude", Bin: o.Bin, Flags: o.flags(), ReadOnly: readOnlyFlags(),
	}
}

func (o Options) Review(ctx context.Context, env envexec.Env, timeout time.Duration, baseRef, rules, outFile string, log io.Writer) error {
	return o.cli().Review(ctx, env, timeout, baseRef, rules, outFile, log)
}

// Prompt runs a read-only pass: aggregation, verification, the PR
// description and the divergence analysis.
func (o Options) Prompt(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	return o.cli().Prompt(ctx, env, timeout, prompt, outFile, stdin, log)
}

// Exec starts a new resumable session, writes the final message to outFile
// and returns the session id from Claude's JSON envelope. Sessions persist in
// the worktree's project scope, which is what makes Resume from the same
// worktree - and only from there - find it again.
func (o Options) Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (string, error) {
	return o.cli().Exec(ctx, env, timeout, prompt, outFile, log)
}

func (o Options) Resume(ctx context.Context, env envexec.Env, timeout time.Duration, sessionID, prompt, outFile string, log io.Writer) error {
	return o.cli().Resume(ctx, env, timeout, sessionID, prompt, outFile, log)
}

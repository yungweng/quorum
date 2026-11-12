// Package grokcli drives the Grok CLI as an engine, mirroring what
// internal/claudecli does for Claude Code: flag construction, the review-side
// passes and the resumable fix session. Like Claude, Grok has no built-in
// review mode, so the reviewer prompt is quorum's own; it prints a JSON
// envelope on stdout; and the session id comes back in that envelope.
package grokcli

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/headless"
)

// Options are the model settings for one Grok invocation.
type Options struct {
	Bin   string // the grok binary to run
	Model string // empty leaves Grok's own default

	// Effort is the reasoning effort, one of the levels the engine table
	// lists for grok; empty leaves Grok's own default. Whether a given model
	// honours it is the CLI's business.
	Effort string

	// Bypass adds --always-approve (permission-mode bypassPermissions). The
	// fix sessions need it: they run tests, use gh and push, unattended. The
	// review side never sets it and runs with a fixed read-only tool set
	// instead.
	Bypass bool
}

func (o Options) flags() []string {
	f := []string{"--output-format", "json"}
	if o.Model != "" {
		f = append(f, "--model", o.Model)
	}
	if o.Effort != "" {
		f = append(f, "--effort", o.Effort)
	}
	if o.Bypass {
		f = append(f, "--always-approve")
	}
	return f
}

// readOnlyFlags is the review-side posture: only the read tools exist for the
// model, web and subagents stay off, anything that would prompt is auto-denied,
// and the OS sandbox refuses writes to the project tree.
func readOnlyFlags() []string {
	return []string{
		"--tools", "read_file,grep,list_dir",
		"--permission-mode", "dontAsk",
		"--sandbox", "read-only",
		"--disable-web-search",
		"--disallowed-tools", "Agent",
	}
}

// resultEnvelope is grok --output-format json's stdout schema; the fields
// quorum needs from it, everything else is ignored.
type resultEnvelope struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionId"`
	// Type is set to "error" on CLI validation failures and similar.
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Answer, Session and Failure read the envelope for headless.CLI. An error
// envelope without a message falls back to the first line of the raw answer.
func (e resultEnvelope) Answer() string  { return e.Text }
func (e resultEnvelope) Session() string { return e.SessionID }
func (e resultEnvelope) Failure(stdout []byte) (string, bool) {
	if e.Type != "error" {
		return "", false
	}
	if e.Message == "" {
		return headless.FirstLine(string(stdout)), true
	}
	return e.Message, true
}

// cli is the headless runner with these settings. The prompt goes in through
// --prompt-file.
func (o Options) cli() headless.CLI[resultEnvelope] {
	return headless.CLI[resultEnvelope]{
		Name: "grok", Bin: o.Bin, Flags: o.flags(), ReadOnly: readOnlyFlags(),
		Deliver: promptFile,
	}
}

// promptFile passes the prompt as --prompt-file, after every other argument,
// with nothing on stdin.
func promptFile(args []string, prompt io.Reader) ([]string, io.Reader, func(), error) {
	path, cleanup, err := writePromptFile(prompt)
	if err != nil {
		return nil, nil, nil, err
	}
	return append(args, "--prompt-file", path), nil, cleanup, nil
}

// writePromptFile materialises prompt into a temp file for --prompt-file.
// Grok does not take the user prompt on stdin in headless mode.
func writePromptFile(prompt io.Reader) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "quorum-grok-prompt-*.txt")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if _, err := io.Copy(f, prompt); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
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
// and returns the session id from Grok's JSON envelope. Sessions persist
// under ~/.grok/sessions keyed by the worktree path, which is what makes
// Resume from the same worktree - and only from there - find it again.
func (o Options) Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (string, error) {
	return o.cli().Exec(ctx, env, timeout, prompt, outFile, log)
}

func (o Options) Resume(ctx context.Context, env envexec.Env, timeout time.Duration, sessionID, prompt, outFile string, log io.Writer) error {
	return o.cli().Resume(ctx, env, timeout, sessionID, prompt, outFile, log)
}

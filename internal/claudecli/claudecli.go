// Package claudecli drives the Claude Code CLI as an engine, mirroring what
// internal/codex does for Codex: flag construction, the review-side passes
// and the resumable fix session. The differences are shape, not policy:
// Claude has no built-in review mode, so the reviewer prompt is quorum's own;
// it prints its result as a JSON envelope on stdout instead of writing a file;
// and it hands the session id back directly instead of leaving it to a
// rollout scan.
package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/usagelimit"
)

// Options are the model settings for one Claude invocation.
type Options struct {
	Bin   string // resolved claude binary; empty falls back to a PATH lookup
	Model string // empty leaves Claude Code's own default

	// Effort exists for interface parity with the Codex engine. The Claude
	// Code CLI has no reasoning-effort flag, so it is ignored.
	Effort string

	// Bypass adds --dangerously-skip-permissions. The fix sessions need it:
	// they run tests, use gh and push, unattended. The review side never sets
	// it and runs with a fixed read-only tool set instead.
	Bypass bool
}

func (o Options) bin() string {
	if o.Bin == "" {
		return "claude"
	}
	return o.Bin
}

// flags builds the shared part of every invocation.
func (o Options) flags() []string {
	f := []string{"-p", "--output-format", "json"}
	if o.Model != "" {
		f = append(f, "--model", o.Model)
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

// run executes claude with the prompt on stdin, parses the JSON envelope from
// stdout, writes the result text to outFile and returns the envelope. The raw
// stdout and stderr both go to the caller's log, and a nonzero exit or an
// is_error envelope is classified as a usage-limit refusal when the output
// says so, exactly like the Codex engine's run helper.
func (o Options) run(ctx context.Context, env envexec.Env, timeout time.Duration, args []string, prompt io.Reader, outFile string, log io.Writer) (resultEnvelope, error) {
	var stdout bytes.Buffer
	var tail usagelimit.Tail
	err := env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdin: prompt,
		Stdout: io.MultiWriter(&stdout, &tail),
		Stderr: io.MultiWriter(log, &tail),
	})
	if stdout.Len() > 0 {
		fmt.Fprintf(log, "%s\n", stdout.String())
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ul := classify(tail.String()); ul != nil {
				return resultEnvelope{}, ul
			}
		}
		return resultEnvelope{}, err
	}

	var envl resultEnvelope
	if jerr := json.Unmarshal(stdout.Bytes(), &envl); jerr != nil {
		return resultEnvelope{}, fmt.Errorf("claude did not answer with its JSON envelope: %v", jerr)
	}
	if envl.IsError {
		if ul := classify(envl.Result); ul != nil {
			return envl, ul
		}
		return envl, fmt.Errorf("claude reported an error: %s", firstLine(envl.Result))
	}
	if outFile != "" {
		if werr := os.WriteFile(outFile, []byte(envl.Result), 0o644); werr != nil {
			return envl, werr
		}
	}
	return envl, nil
}

// classify recognises Claude's quota refusals. Unlike Codex there is no
// single message to match, so this goes by the two phrasings the CLI uses;
// no reset time is parsed because the messages do not carry a full date.
func classify(output string) *usagelimit.Error {
	lower := strings.ToLower(output)
	for _, marker := range []string{"usage limit", "rate limit"} {
		if !strings.Contains(lower, marker) {
			continue
		}
		line := output
		for l := range strings.SplitSeq(output, "\n") {
			if strings.Contains(strings.ToLower(l), marker) {
				line = strings.TrimSpace(l)
				break
			}
		}
		return &usagelimit.Error{Line: line}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// diffCap bounds how much diff is inlined into the reviewer prompt. Claude
// caps stdin at 10MB; past this the prompt falls back to a diffstat and the
// reviewer reads the changed files through its tools instead.
const diffCap = 2 * 1024 * 1024

// Review runs one reviewer pass and writes its findings to outFile. Codex
// ships a review subcommand that computes the diff itself; Claude does not,
// so the diff against baseRef is computed here and handed over inline.
func (o Options) Review(ctx context.Context, env envexec.Env, timeout time.Duration, baseRef, outFile string, log io.Writer) error {
	diff, err := reviewDiff(ctx, env, baseRef, log)
	if err != nil {
		return err
	}
	prompt := reviewPrompt(baseRef, diff)
	args := append(o.flags(), readOnlyFlags()...)
	_, err = o.run(ctx, env, timeout, args, strings.NewReader(prompt), outFile, log)
	return err
}

// reviewDiff returns the merge-base diff against baseRef, or the diffstat
// when the full diff would blow the prompt budget.
func reviewDiff(ctx context.Context, env envexec.Env, baseRef string, log io.Writer) (string, error) {
	full, err := gitDiff(ctx, env, log, baseRef+"...HEAD")
	if err != nil {
		return "", fmt.Errorf("computing the diff against %s: %w", baseRef, err)
	}
	if len(full) <= diffCap {
		return full, nil
	}
	stat, err := gitDiff(ctx, env, log, "--stat", baseRef+"...HEAD")
	if err != nil {
		return "", fmt.Errorf("computing the diffstat against %s: %w", baseRef, err)
	}
	return "The full diff is too large to inline. Its diffstat follows; read the listed files with your tools to see the changes.\n\n" + stat, nil
}

func gitDiff(ctx context.Context, env envexec.Env, log io.Writer, args ...string) (string, error) {
	var out bytes.Buffer
	err := env.Run(ctx, 2*time.Minute, envexec.Cmd{
		Name: "git", Args: append([]string{"diff"}, args...), Stdout: &out, Stderr: log,
	})
	return out.String(), err
}

// Aggregate runs the aggregator pass: read-only, prompt as instruction and
// the collected reviewer outputs appended behind it.
func (o Options) Aggregate(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	return o.promptPass(ctx, env, timeout, prompt, outFile, stdin, log)
}

// Verify runs the independent evidence pass with the same read-only posture.
func (o Options) Verify(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	return o.promptPass(ctx, env, timeout, prompt, outFile, stdin, log)
}

// DescribePR writes a final-state pull request description, read-only like
// the other prompt passes.
func (o Options) DescribePR(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	return o.promptPass(ctx, env, timeout, prompt, outFile, stdin, log)
}

func (o Options) promptPass(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	input := io.Reader(strings.NewReader(prompt))
	if stdin != nil {
		input = io.MultiReader(strings.NewReader(prompt+"\n\n"), stdin)
	}
	args := append(o.flags(), readOnlyFlags()...)
	_, err := o.run(ctx, env, timeout, args, input, outFile, log)
	return err
}

// Exec starts a new resumable session, writes the final message to outFile
// and returns the session id from Claude's JSON envelope. Sessions persist in
// the worktree's project scope, which is what makes Resume from the same
// worktree - and only from there - find it again.
func (o Options) Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (string, error) {
	envl, err := o.run(ctx, env, timeout, o.flags(), strings.NewReader(prompt), outFile, log)
	if err != nil {
		return "", err
	}
	if envl.SessionID == "" {
		return "", fmt.Errorf("claude's answer carried no session id, cannot resume later")
	}
	return envl.SessionID, nil
}

// Resume continues an existing session, so context carries from the CI fixes
// through every review round. It must run in the worktree the session started
// in; envexec pins the working directory there.
func (o Options) Resume(ctx context.Context, env envexec.Env, timeout time.Duration, sessionID, prompt, outFile string, log io.Writer) error {
	args := append(o.flags(), "--resume", sessionID)
	_, err := o.run(ctx, env, timeout, args, strings.NewReader(prompt), outFile, log)
	return err
}

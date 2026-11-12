// Package headless is what the Claude Code and Grok engines share: both run
// a CLI in headless mode that prints a JSON envelope on stdout, neither has a
// built-in review mode, and both resume a session by id. The adapters keep
// what is truly their own - flags, the read-only posture, how the prompt is
// delivered and how the envelope is decoded - and hand the rest to CLI.
package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/usagelimit"
)

// Envelope is the part of a CLI's JSON answer CLI reads.
type Envelope interface {
	// Answer is the final message, written to the caller's outFile.
	Answer() string
	// Session is the id Resume takes back.
	Session() string
	// Failure returns the error text when the envelope reports a failure.
	// stdout is the raw answer, for CLIs whose error envelope may lack one.
	Failure(stdout []byte) (string, bool)
}

// Deliver hands the prompt to the CLI. It returns the final argument list,
// the process's stdin and a cleanup to run once the process has exited.
type Deliver func(args []string, prompt io.Reader) ([]string, io.Reader, func(), error)

// CLI is one headless engine with its model settings already applied.
type CLI[E Envelope] struct {
	Name     string   // the CLI's name in error messages: "claude"
	Bin      string   // the binary to run
	Flags    []string // every invocation
	ReadOnly []string // the review-side posture, added for read-only passes
	// Deliver passes the prompt; nil feeds it on stdin.
	Deliver Deliver
}

// Review runs one reviewer pass and writes its findings to outFile. Codex
// ships a review subcommand that computes the diff itself; the headless CLIs
// do not, so the diff against baseRef is computed here and handed over inline.
func (c CLI[E]) Review(ctx context.Context, env envexec.Env, timeout time.Duration, baseRef, rules, outFile string, log io.Writer) error {
	diff, err := reviewDiff(ctx, env, baseRef, log)
	if err != nil {
		return err
	}
	prompt := reviewPrompt(baseRef, diff, rules)
	_, err = c.run(ctx, env, timeout, c.readOnly(), strings.NewReader(prompt), outFile, log)
	return err
}

// Prompt runs a read-only pass: prompt as instruction and stdin, when there
// is one, appended behind it.
func (c CLI[E]) Prompt(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	input := io.Reader(strings.NewReader(prompt))
	if stdin != nil {
		input = io.MultiReader(strings.NewReader(prompt+"\n\n"), stdin)
	}
	_, err := c.run(ctx, env, timeout, c.readOnly(), input, outFile, log)
	return err
}

// Exec starts a new resumable session, writes the final message to outFile
// and returns the session id from the JSON envelope.
func (c CLI[E]) Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (string, error) {
	envl, err := c.run(ctx, env, timeout, slices.Clone(c.Flags), strings.NewReader(prompt), outFile, log)
	if err != nil {
		return "", err
	}
	if envl.Session() == "" {
		return "", fmt.Errorf("%s's answer carried no session id, cannot resume later", c.Name)
	}
	return envl.Session(), nil
}

// Resume continues an existing session, so context carries from the CI fixes
// through every review round. It must run in the worktree the session started
// in; envexec pins the working directory there.
func (c CLI[E]) Resume(ctx context.Context, env envexec.Env, timeout time.Duration, sessionID, prompt, outFile string, log io.Writer) error {
	args := append(slices.Clone(c.Flags), "--resume", sessionID)
	_, err := c.run(ctx, env, timeout, args, strings.NewReader(prompt), outFile, log)
	return err
}

func (c CLI[E]) readOnly() []string {
	return append(slices.Clone(c.Flags), c.ReadOnly...)
}

// run executes the CLI, parses the JSON envelope from stdout, writes the
// answer to outFile and returns the envelope. The raw stdout and stderr both
// go to the caller's log, and a nonzero exit or an error envelope is
// classified as a usage-limit refusal when the output says so, exactly like
// the Codex engine's run helper.
func (c CLI[E]) run(ctx context.Context, env envexec.Env, timeout time.Duration, args []string, prompt io.Reader, outFile string, log io.Writer) (E, error) {
	var zero E
	stdin := prompt
	if c.Deliver != nil {
		var cleanup func()
		var err error
		args, stdin, cleanup, err = c.Deliver(args, prompt)
		if err != nil {
			return zero, err
		}
		defer cleanup()
	}

	var stdout bytes.Buffer
	var tail usagelimit.Tail
	// Only stderr feeds the refusal tail. Stdout carries the JSON envelope,
	// whose result text quotes whatever the model just read - including code
	// or docs that mention usage limits - while the CLI keeps stdout parseable
	// in JSON mode and reports its errors on stderr.
	err := env.Run(ctx, timeout, envexec.Cmd{
		Name: c.Bin, Args: args, Stdin: stdin,
		Stdout: &stdout,
		Stderr: io.MultiWriter(log, &tail),
	})
	if stdout.Len() > 0 {
		fmt.Fprintf(log, "%s\n", stdout.String())
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ul := classify(tail.String()); ul != nil {
				return zero, ul
			}
		}
		return zero, err
	}

	var envl E
	if jerr := json.Unmarshal(stdout.Bytes(), &envl); jerr != nil {
		return zero, fmt.Errorf("%s did not answer with its JSON envelope: %v", c.Name, jerr)
	}
	if msg, failed := envl.Failure(stdout.Bytes()); failed {
		if ul := classify(msg); ul != nil {
			return envl, ul
		}
		return envl, fmt.Errorf("%s reported an error: %s", c.Name, FirstLine(msg))
	}
	if outFile != "" {
		if werr := os.WriteFile(outFile, []byte(envl.Answer()), 0o644); werr != nil {
			return envl, werr
		}
	}
	return envl, nil
}

// classify recognises a quota refusal. Only "usage limit" phrasings count as
// account exhaustion: a transient per-minute throttle says "rate limit" too,
// and with every reviewer starting at once that is routine and must fail like
// any other error instead of pausing the agent for an hour. The match is
// anchored to the last lines, so quoted mentions of the phrase earlier in an
// error text stay unclassified. No reset time is parsed because the messages
// do not carry a full date.
func classify(output string) *usagelimit.Error {
	line, ok := usagelimit.RefusalLine(output, "usage limit")
	if !ok {
		return nil
	}
	return &usagelimit.Error{Line: line}
}

func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// diffCap bounds how much diff is inlined into the reviewer prompt. Claude
// caps stdin at 10MB; past this the prompt falls back to a diffstat and the
// reviewer reads the changed files through its tools instead.
const diffCap = 2 * 1024 * 1024

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

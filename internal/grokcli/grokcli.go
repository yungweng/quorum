// Package grokcli drives the Grok CLI as an engine, mirroring what
// internal/claudecli does for Claude Code: flag construction, the review-side
// passes and the resumable fix session. Like Claude, Grok has no built-in
// review mode, so the reviewer prompt is quorum's own; it prints a JSON
// envelope on stdout; and the session id comes back in that envelope.
package grokcli

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

// Efforts is the set of reasoning-effort values the Grok CLI accepts for its
// default model menu (checked against 1.0.0: low, medium, high). Unknown
// levels fail at the CLI with a JSON error; validating here keeps a typo from
// starting a full panel.
var Efforts = []string{"low", "medium", "high"}

// ValidEffort reports whether e is one of Efforts. The empty string is allowed
// and means "leave Grok's own default alone".
func ValidEffort(e string) bool {
	return e == "" || slices.Contains(Efforts, e)
}

// Options are the model settings for one Grok invocation.
type Options struct {
	Bin   string // resolved grok binary; empty falls back to a PATH lookup
	Model string // empty leaves Grok's own default

	// Effort is the reasoning effort, one of Efforts; empty leaves Grok's own
	// default. Whether a given model honours it is the CLI's business.
	Effort string

	// Bypass adds --always-approve (permission-mode bypassPermissions). The
	// fix sessions need it: they run tests, use gh and push, unattended. The
	// review side never sets it and runs with a fixed read-only tool set
	// instead.
	Bypass bool
}

func (o Options) bin() string {
	if o.Bin == "" {
		return "grok"
	}
	return o.Bin
}

// flags builds the shared part of every invocation.
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

// run writes prompt to a temp file, executes grok with --prompt-file, parses
// the JSON envelope from stdout, writes the result text to outFile and returns
// the envelope. The raw stdout and stderr both go to the caller's log.
func (o Options) run(ctx context.Context, env envexec.Env, timeout time.Duration, args []string, prompt io.Reader, outFile string, log io.Writer) (resultEnvelope, error) {
	promptPath, cleanup, err := writePromptFile(prompt)
	if err != nil {
		return resultEnvelope{}, err
	}
	defer cleanup()

	args = append(args, "--prompt-file", promptPath)

	var stdout bytes.Buffer
	var tail usagelimit.Tail
	// Only stderr feeds the refusal tail. Stdout carries the JSON envelope,
	// whose result text quotes whatever the model just read - including code
	// or docs that mention usage limits - while the CLI keeps stdout parseable
	// in JSON mode and reports its errors on stderr.
	err = env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args,
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
				return resultEnvelope{}, ul
			}
		}
		return resultEnvelope{}, err
	}

	var envl resultEnvelope
	if jerr := json.Unmarshal(stdout.Bytes(), &envl); jerr != nil {
		return resultEnvelope{}, fmt.Errorf("grok did not answer with its JSON envelope: %v", jerr)
	}
	if envl.Type == "error" {
		msg := envl.Message
		if msg == "" {
			msg = firstLine(stdout.String())
		}
		if ul := classify(msg); ul != nil {
			return envl, ul
		}
		return envl, fmt.Errorf("grok reported an error: %s", firstLine(msg))
	}
	if outFile != "" {
		if werr := os.WriteFile(outFile, []byte(envl.Text), 0o644); werr != nil {
			return envl, werr
		}
	}
	return envl, nil
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

// classify recognises Grok quota refusals. The match is anchored to the last
// lines so quoted mentions earlier in an error text stay unclassified.
func classify(output string) *usagelimit.Error {
	line, ok := usagelimit.RefusalLine(output, "usage limit")
	if !ok {
		return nil
	}
	return &usagelimit.Error{Line: line}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// diffCap bounds how much diff is inlined into the reviewer prompt. Past this
// the prompt falls back to a diffstat and the reviewer reads the changed files
// through its tools instead.
const diffCap = 2 * 1024 * 1024

// Review runs one reviewer pass and writes its findings to outFile. Codex
// ships a review subcommand that computes the diff itself; Grok does not,
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
// and returns the session id from Grok's JSON envelope. Sessions persist
// under ~/.grok/sessions keyed by the worktree path, which is what makes
// Resume from the same worktree - and only from there - find it again.
func (o Options) Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (string, error) {
	envl, err := o.run(ctx, env, timeout, o.flags(), strings.NewReader(prompt), outFile, log)
	if err != nil {
		return "", err
	}
	if envl.SessionID == "" {
		return "", fmt.Errorf("grok's answer carried no session id, cannot resume later")
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

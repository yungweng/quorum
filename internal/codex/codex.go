// Package codex drives the Codex CLI: flag construction, the three exec forms
// quorum uses, and recovery of a session id so later steps can resume it.
package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/usagelimit"
)

// Efforts is the set of reasoning-effort values Codex accepts. Validating this
// before spawning anything is deliberate: Codex silently ignores an unknown
// effort instead of failing, so a typo would quietly run every reviewer at the
// wrong setting and only show up in the bill.
var Efforts = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// ValidEffort reports whether e is one of Efforts. The empty string is allowed
// and means "leave the user's Codex default alone".
func ValidEffort(e string) bool {
	return e == "" || slices.Contains(Efforts, e)
}

// Options are the model settings for one Codex invocation.
type Options struct {
	Bin    string // resolved codex binary; empty falls back to a PATH lookup
	Model  string // empty leaves the user's Codex default
	Effort string // empty leaves the user's Codex default

	// Bypass adds --dangerously-bypass-approvals-and-sandbox. The fix sessions
	// need it: they run tests, use gh and push, unattended, and a sandboxed or
	// approval-gated exec would silently skip exactly those commands. The
	// review side never sets it: reviewers and aggregation run read-only, while
	// verification also uses the read-only sandbox.
	Bypass bool

	// DisableSerena turns the Serena MCP server off. It adds nothing to a diff
	// review and its per-reviewer server spawns have deadlocked runs before
	// (hung write_memory, duplicate instances).
	DisableSerena bool
}

func (o Options) bin() string {
	if o.Bin == "" {
		return "codex"
	}
	return o.Bin
}

// flags builds the shared part of every invocation.
func (o Options) flags() []string {
	var f []string
	if o.Model != "" {
		f = append(f, "-m", o.Model)
	}
	if o.Effort != "" {
		f = append(f, "-c", "model_reasoning_effort="+o.Effort)
	}
	if o.DisableSerena {
		f = append(f, "-c", "mcp_servers.serena.enabled=false")
	}
	if o.Bypass {
		f = append(f, "--dangerously-bypass-approvals-and-sandbox")
	}
	return f
}

// reviewFlags adds the review-specific model override. Codex has a separate
// review_model setting that takes precedence over -m, so both have to be set
// or a user's config could silently select a different model for the reviewers
// than the one that was asked for and paid for.
func (o Options) reviewFlags() []string {
	f := o.flags()
	if o.Model != "" {
		f = append(f, "-c", "review_model="+TOMLString(o.Model))
	}
	return f
}

// TOMLString quotes s as a TOML basic string for `codex -c key=value`.
// The shell version delegated this to `jq -Rn '$model'`; the escape rules that
// matter here (quote, backslash, control characters) are identical between
// JSON and TOML basic strings.
func TOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// usageLimitLine is Codex's refusal when the account has no quota left. The
// full message names the reset time, which run passes along.
var usageLimitLine = "hit your usage limit"

// run is the single place every Codex invocation goes through. It tees the
// combined output into a bounded tail beside the caller's log and, when Codex
// itself exits nonzero, classifies a usage-limit refusal into the typed error
// the agent and the fix loop branch on. A timeout, a cancelled context or a
// missing binary are not classified: only an actual exit is a message from
// Codex.
func (o Options) run(ctx context.Context, env envexec.Env, timeout time.Duration, args []string, stdin io.Reader, log io.Writer) error {
	var tail usagelimit.Tail
	// One writer value for both streams, so os/exec merges them into a single
	// pipe exactly as the previous Stdout=Stderr=log wiring did.
	w := io.MultiWriter(log, &tail)
	err := env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdin: stdin, Stdout: w, Stderr: w,
	})
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	out := tail.String()
	if !strings.Contains(strings.ToLower(out), usageLimitLine) {
		return err
	}
	line := out
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(strings.ToLower(l), usageLimitLine) {
			line = strings.TrimSpace(l)
			break
		}
	}
	return &usagelimit.Error{ResetAt: usagelimit.ParseResetAt(line), Line: line}
}

// Review runs `codex exec review` against baseRef and writes the reviewer's
// findings to outFile. --ephemeral keeps the run out of the session history:
// reviewers are throwaway and nothing ever resumes them.
func (o Options) Review(ctx context.Context, env envexec.Env, timeout time.Duration, baseRef, outFile string, log io.Writer) error {
	args := append([]string{"exec", "review"}, o.reviewFlags()...)
	args = append(args, "--base", baseRef, "--ephemeral", "-o", outFile)
	return o.run(ctx, env, timeout, args, nil, log)
}

// Aggregate runs the aggregator pass: read-only, ephemeral, prompt as the
// argument and the collected reviewer outputs on stdin.
func (o Options) Aggregate(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "--sandbox", "read-only", "--ephemeral", "-o", outFile, prompt)
	return o.run(ctx, env, timeout, args, stdin, log)
}

// Verify runs the independent evidence pass in the same read-only sandbox as
// aggregation. The candidate report arrives on stdin and the verified report
// is the final message written to outFile.
func (o Options) Verify(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "--sandbox", "read-only", "--ephemeral", "-o", outFile, prompt)
	return o.run(ctx, env, timeout, args, stdin, log)
}

// DescribePR writes a final-state pull request description. It is separate
// from the resumable fix session so the output describes the finished diff
// without inheriting the round-by-round conversation.
func (o Options) DescribePR(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "--sandbox", "read-only", "--ephemeral", "-o", outFile, prompt)
	return o.run(ctx, env, timeout, args, stdin, log)
}

// Exec starts a new resumable session, writes the final message to outFile
// and returns the session id recovered from ~/.codex/sessions. The lookup is
// keyed on the worktree path, which is unique per run; see FindSession.
func (o Options) Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (string, error) {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "-o", outFile, prompt)
	if err := o.run(ctx, env, timeout, args, nil, log); err != nil {
		return "", err
	}
	id, err := FindSession(env.Worktree)
	if err != nil {
		return "", fmt.Errorf("could not find the Codex session for %s: %w", env.Worktree, err)
	}
	return id, nil
}

// Resume continues an existing session, so context carries from the CI fixes
// through every review round.
func (o Options) Resume(ctx context.Context, env envexec.Env, timeout time.Duration, sessionID, prompt, outFile string, log io.Writer) error {
	args := append([]string{"exec", "resume", sessionID}, o.flags()...)
	args = append(args, "-o", outFile, prompt)
	return o.run(ctx, env, timeout, args, nil, log)
}

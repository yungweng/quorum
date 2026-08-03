// Package codex drives the Codex CLI: flag construction, the three exec forms
// quorum uses, and recovery of a session id so later steps can resume it.
package codex

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
)

// Efforts is the set of reasoning-effort values Codex accepts. Validating this
// before spawning anything is deliberate: Codex silently ignores an unknown
// effort instead of failing, so a typo would quietly run every reviewer at the
// wrong setting and only show up in the bill.
var Efforts = []string{"minimal", "low", "medium", "high", "xhigh"}

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

// Review runs `codex exec review` against baseRef and writes the reviewer's
// findings to outFile. --ephemeral keeps the run out of the session history:
// reviewers are throwaway and nothing ever resumes them.
func (o Options) Review(ctx context.Context, env envexec.Env, timeout time.Duration, baseRef, outFile string, log io.Writer) error {
	args := append([]string{"exec", "review"}, o.reviewFlags()...)
	args = append(args, "--base", baseRef, "--ephemeral", "-o", outFile)
	return env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdout: log, Stderr: log,
	})
}

// Aggregate runs the aggregator pass: read-only, ephemeral, prompt as the
// argument and the collected reviewer outputs on stdin.
func (o Options) Aggregate(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "--sandbox", "read-only", "--ephemeral", "-o", outFile, prompt)
	return env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdin: stdin, Stdout: log, Stderr: log,
	})
}

// Verify runs the independent evidence pass in the same read-only sandbox as
// aggregation. The candidate report arrives on stdin and the verified report
// is the final message written to outFile.
func (o Options) Verify(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "--sandbox", "read-only", "--ephemeral", "-o", outFile, prompt)
	return env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdin: stdin, Stdout: log, Stderr: log,
	})
}

// DescribePR writes a final-state pull request description. It is separate
// from the resumable fix session so the output describes the finished diff
// without inheriting the round-by-round conversation.
func (o Options) DescribePR(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "--sandbox", "read-only", "--ephemeral", "-o", outFile, prompt)
	return env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdin: stdin, Stdout: log, Stderr: log,
	})
}

// Exec starts a new resumable session and writes the final message to outFile.
func (o Options) Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) error {
	args := append([]string{"exec"}, o.flags()...)
	args = append(args, "-o", outFile, prompt)
	return env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdout: log, Stderr: log,
	})
}

// Resume continues an existing session, so context carries from the CI fixes
// through every review round.
func (o Options) Resume(ctx context.Context, env envexec.Env, timeout time.Duration, sessionID, prompt, outFile string, log io.Writer) error {
	args := append([]string{"exec", "resume", sessionID}, o.flags()...)
	args = append(args, "-o", outFile, prompt)
	return env.Run(ctx, timeout, envexec.Cmd{
		Name: o.bin(), Args: args, Stdout: log, Stderr: log,
	})
}

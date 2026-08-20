// Package git wraps the git commands quorum needs.
//
// Everything runs against an explicit directory rather than relying on the
// process working directory: the review core and the fix pipeline both juggle
// the user's checkout and a detached worktree at the same time, and a command
// that lands in the wrong one is exactly the mistake that touches somebody's
// uncommitted work.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yungweng/quorum/internal/cachefs"
)

// G runs git from a resolved binary.
type G struct {
	Bin string
}

func New(bin string) G {
	if bin == "" {
		bin = "git"
	}
	return G{Bin: bin}
}

// run executes git in dir and returns trimmed stdout.
func (g G) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), firstLine(msg))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runCombined is for commands whose output matters even when they fail, such as
// a push whose pre-push hook printed the reason.
func (g G) runCombined(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Toplevel resolves the repository root containing dir.
func (g G) Toplevel(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--show-toplevel")
}

// RevParse resolves a revision to a full SHA.
func (g G) RevParse(ctx context.Context, dir, rev string) (string, error) {
	return g.run(ctx, dir, "rev-parse", rev)
}

// CurrentBranch returns the checked-out branch name, or "HEAD" when detached.
func (g G) CurrentBranch(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// Fetch runs a fetch with an explicit refspec.
func (g G) Fetch(ctx context.Context, dir, remote, refspec string) error {
	_, err := g.run(ctx, dir, "fetch", "-q", remote, refspec)
	return err
}

// LsRemote asks the remote what a ref currently points at, without fetching.
// This is how head drift is detected: the local worktree cannot answer it.
func (g G) LsRemote(ctx context.Context, dir, remote, ref string) (string, error) {
	out, err := g.run(ctx, dir, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	sha, _, ok := strings.Cut(out, "\t")
	if !ok || sha == "" {
		return "", fmt.Errorf("could not resolve %s on %s", ref, remote)
	}
	return sha, nil
}

// ShowFile returns a tracked file's content at rev. ok is false when the path
// does not exist there; only a real read failure is an error.
func (g G) ShowFile(ctx context.Context, dir, rev, path string) (string, bool, error) {
	if _, err := g.run(ctx, dir, "rev-parse", "-q", "--verify", rev+"^{tree}"); err != nil {
		return "", false, err
	}
	entry, err := g.run(ctx, dir, "ls-tree", "--name-only", rev, "--", path)
	if err != nil {
		return "", false, err
	}
	if entry == "" {
		return "", false, nil
	}
	out, err := g.run(ctx, dir, "show", rev+":"+path)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// WorktreeAdd creates a detached worktree at path checked out at sha.
func (g G) WorktreeAdd(ctx context.Context, repoRoot, path, sha string) error {
	_, err := g.run(ctx, repoRoot, "worktree", "add", "--quiet", "--detach", path, sha)
	return err
}

// WorktreeRemove drops a worktree. Git can fail on dependency managers' read-
// only directories, so cache-owned worktrees get a filesystem fallback. The
// regular repository sweeps prune a registration left by that fallback.
func (g G) WorktreeRemove(ctx context.Context, repoRoot, path string) {
	if _, err := g.run(ctx, repoRoot, "worktree", "remove", path, "--force"); err != nil {
		_ = cachefs.RemoveAll(path)
	}
}

// WorktreePrune clears worktree registrations whose directories are gone.
func (g G) WorktreePrune(ctx context.Context, repoRoot string) {
	_, _ = g.run(ctx, repoRoot, "worktree", "prune")
}

// Dirty reports whether the working tree has uncommitted changes.
func (g G) Dirty(ctx context.Context, dir string) (bool, error) {
	out, err := g.StatusPorcelain(ctx, dir)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// StatusPorcelain returns the stable, machine-readable working tree status.
func (g G) StatusPorcelain(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "status", "--porcelain")
}

// StatusPorcelainTracked returns staged and unstaged changes to tracked files,
// excluding untracked paths. Callers use it when generated files may be safe
// to discard but changes to the checked-out source are not.
func (g G) StatusPorcelainTracked(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "status", "--porcelain", "--untracked-files=no")
}

// CleanUntracked removes untracked files and directories while preserving
// ignored paths. It is deliberately narrower than reset: tracked changes are
// evidence of a bad run and must remain available for diagnosis.
func (g G) CleanUntracked(ctx context.Context, dir string) error {
	_, err := g.run(ctx, dir, "clean", "-fd")
	return err
}

// LogOneline lists the commits in a range, one per line, following only the
// first parent. A fix round that merged the base branch would otherwise list
// every commit the merge brought in - other PRs' work - as its own; with
// --first-parent that merge stays a single line.
func (g G) LogOneline(ctx context.Context, dir, revRange string) string {
	out, _ := g.run(ctx, dir, "log", "--oneline", "--first-parent", revRange)
	return out
}

// Push pushes HEAD to a branch on the remote and returns the combined output.
//
// The detached worktree is why the refspec is explicit: a bare `git push` fails
// on a detached HEAD, which is also why the Codex fix sessions are told the
// exact command in their standing rules.
func (g G) Push(ctx context.Context, dir, remote, branch string) (string, error) {
	return g.runCombined(ctx, dir, "push", "-q", remote, "HEAD:refs/heads/"+branch)
}

// PrePush runs the configured pre-push hook against HEAD.  A failed push by
// itself cannot tell a local hook failure from a remote rejection; this probe
// can.
func (g G) PrePush(ctx context.Context, dir, remote, branch, sha, remoteSHA string) (string, error) {
	input, err := os.CreateTemp("", "quorum-pre-push-*")
	if err != nil {
		return "", err
	}
	path := input.Name()
	defer os.Remove(path)
	if _, err := fmt.Fprintf(input, "HEAD %s refs/heads/%s %s\n", sha, branch, remoteSHA); err != nil {
		input.Close()
		return "", err
	}
	if err := input.Close(); err != nil {
		return "", err
	}
	url, err := g.run(ctx, dir, "remote", "get-url", "--push", remote)
	if err != nil {
		return "", err
	}
	return g.runCombined(ctx, dir, "hook", "run", "--ignore-missing", "--to-stdin="+filepath.Clean(path), "pre-push", "--", remote, url)
}

// PrePushPath resolves the configured pre-push hook path.
func (g G) PrePushPath(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-push")
}

// MergeConflicts reports whether merging head onto base would conflict,
// without touching any working tree. It needs the --write-tree form of
// merge-tree (git 2.38); an older git returns an error, which callers treat
// as "unknown" rather than as an answer.
func (g G) MergeConflicts(ctx context.Context, dir, base, head string) (bool, error) {
	cmd := exec.CommandContext(ctx, g.Bin, "merge-tree", "--write-tree", "--name-only", base, head)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	// Exit 1 covers both "conflicts" and "not something we can merge". Only a
	// real conflict run writes the merged tree's oid to stdout, so an empty
	// stdout means the question could not be answered, not that it was.
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && strings.TrimSpace(stdout.String()) != "" {
		return true, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return false, fmt.Errorf("git merge-tree --write-tree %s %s: %s", base, head, firstLine(msg))
}

// IsAncestor reports whether ancestor is reachable from descendant. It is how
// "the local branch is merely behind origin" is told apart from "the local
// branch carries commits of its own": a fix pipeline pushes to origin without
// ever moving the local ref, so falling behind is routine and loses nothing,
// while own commits would be silently overtaken.
func (g G) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(ctx, g.Bin, "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %v", err)
}

// HasLocalBranch reports whether a local branch of that name exists.
func (g G) HasLocalBranch(ctx context.Context, dir, branch string) bool {
	_, err := g.run(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// ChangedFiles lists paths that differ between two revisions, limited to
// pathspecs when given.
func (g G) ChangedFiles(ctx context.Context, dir, revRange string, pathspecs ...string) (string, error) {
	args := append([]string{"diff", "--name-only", revRange}, appendPathspecs(pathspecs)...)
	return g.run(ctx, dir, args...)
}

// ChangedFilesNoRenames reports both sides of a rename as delete and add.
func (g G) ChangedFilesNoRenames(ctx context.Context, dir, revRange string) (string, error) {
	return g.run(ctx, dir, "diff", "--no-renames", "--name-only", revRange)
}

// Diff renders the diff against a revision, limited to pathspecs when given.
func (g G) Diff(ctx context.Context, dir, rev string, pathspecs ...string) string {
	args := append([]string{"diff", rev}, appendPathspecs(pathspecs)...)
	out, _ := g.run(ctx, dir, args...)
	return out
}

// IsIgnored reports whether a path is covered by gitignore.
func (g G) IsIgnored(ctx context.Context, dir, path string) bool {
	_, err := g.run(ctx, dir, "check-ignore", "-q", "--", path)
	return err == nil
}

func appendPathspecs(pathspecs []string) []string {
	if len(pathspecs) == 0 {
		return nil
	}
	return append([]string{"--"}, pathspecs...)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

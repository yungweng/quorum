package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/proc"
)

// testLogTailLines bounds how much test output goes into a fix prompt. The
// full log stays in the run directory.
const testLogTailLines = 80

// RepoTestCmdPath is the tracked file a repository can define its test gate
// in, so a team shares one gate instead of each machine configuring its own.
const RepoTestCmdPath = ".quorum/testcmd"

// resolveRepoTestCmd reads the repository's own test command from the base
// branch, never from the change under review: the gate runs unsandboxed on
// this machine, and reading it out of the diff would let the change under
// babysit weaken or hijack its own gate - the same reasoning as the .envrc
// stop. A command the change edits therefore only applies once it is merged.
func resolveRepoTestCmd(ctx context.Context, gitc git.G, repoRoot, base string) (string, error) {
	if err := gitc.Fetch(ctx, repoRoot, "origin",
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base)); err != nil {
		return "", err
	}
	content, ok, err := gitc.ShowFile(ctx, repoRoot, "origin/"+base, RepoTestCmdPath)
	if err != nil || !ok {
		return "", err
	}
	return strings.TrimSpace(content), nil
}

// ensureTestsGreen is the offline loop's counterpart of ensureCIGreen: it runs
// the configured test command in the worktree and repairs failures through the
// fix session, up to MaxCIFixes attempts per invocation. An empty TestCmd
// means the repository has no deterministic gate; the session prompts still
// demand that affected checks are run.
func (r *run) ensureTestsGreen() error {
	if r.o.TestCmd == "" {
		return nil
	}
	for attempt := 0; ; {
		r.testRuns++
		logPath := filepath.Join(r.logDir, fmt.Sprintf("tests-%d.log", r.testRuns))
		r.enter(PhaseTests)
		r.rep.Step("Tests: " + r.o.TestCmd)
		err := r.runTestCmd(logPath)
		if err == nil {
			if err := r.requireCleanWorktree(fmt.Sprintf("tests-%d", r.testRuns)); err != nil {
				return err
			}
			r.rep.Info("tests green")
			return nil
		}
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		reason := fmt.Sprintf("the test command failed with exit %d", proc.ExitCode(err))
		if errors.Is(err, proc.ErrTimeout) {
			reason = fmt.Sprintf("the test command timed out after %s (--fix-timeout; 0 disables)", r.o.FixTimeout)
		}

		attempt++
		if attempt > r.o.MaxCIFixes {
			r.rep.Notify("Tests rot", fmt.Sprintf("%s nach %d Fix-Versuchen weiter rot", r.targetLabel(), r.o.MaxCIFixes))
			return fmt.Errorf("%w (%d attempts): %s, see %s", ErrTestsRed, r.o.MaxCIFixes, r.o.TestCmd, logPath)
		}
		r.rep.Warn(fmt.Sprintf("%s; starting test fix %d/%d, see %s", reason, attempt, r.o.MaxCIFixes, logPath))

		preSHA, shaErr := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if shaErr != nil {
			return shaErr
		}
		fixNumber := r.testFixTotal + 1
		tag := fmt.Sprintf("test-fix-%d", fixNumber)
		r.enter(PhaseFix)
		if err := r.codexCall(tag, testFixPrompt(r.o.TestCmd, tailLines(logPath, testLogTailLines))); err != nil {
			return err
		}
		if err := r.questionGate(tag); err != nil {
			return err
		}
		if err := r.ensureCommitted(tag); err != nil {
			return err
		}
		r.testFixTotal = fixNumber
		label := fmt.Sprintf("Test fix %d", fixNumber)
		r.recordRound(label, preSHA)
		r.queueFixComment(tag, label, preSHA)
	}
}

// runTestCmd executes TestCmd through the shell inside the worktree, through
// direnv when active, exactly like every other command the pipeline runs
// there. The log always gets everything; --verbose mirrors it to the terminal.
func (r *run) runTestCmd(logPath string) error {
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	var out io.Writer = logFile
	if r.o.Verbose && r.o.Out != nil {
		out = io.MultiWriter(logFile, r.o.Out)
	}
	return r.env.Run(r.ctx, r.o.FixTimeout, envexec.Cmd{
		Name:   "/bin/sh",
		Args:   []string{"-c", r.o.TestCmd},
		Stdout: out,
		Stderr: out,
	})
}

// tailLines returns the last n lines of a log file for a fix prompt.
func tailLines(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(could not read %s: %v)", path, err)
	}
	lines := splitLines(string(data))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimRight(joinLines(lines), "\n")
}

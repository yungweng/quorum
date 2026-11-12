package loop

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yungweng/quorum/internal/deps"
	"github.com/yungweng/quorum/internal/git"
)

// maxPushFixes bounds how often a rejected push is handed to a fix session:
// once for what the repository's verification reported, once for whatever that
// repair itself broke. After that the run stops.
const maxPushFixes = 2

// pushLogTailLines bounds how much of the push output goes into a fix prompt.
// The full output stays in push-last.log.
const pushLogTailLines = 120

// pushRejection is a push that failed while the remote branch stayed where it
// was. It carries the command output, which is what a fix session needs: for a
// repository with pre-push verification that output is the whole complaint.
type pushRejection struct {
	branch string
	sha    string
	out    string
	// local means the pre-push verification refused before any transport;
	// out is then the hook's own output.
	local bool
}

func (e *pushRejection) Error() string {
	if e.local {
		return fmt.Sprintf("pre-push verification failed for %s at %s:\n%s", e.branch, e.sha, e.out)
	}
	return fmt.Sprintf("git push failed and origin/%s does not have %s:\n%s", e.branch, e.sha, e.out)
}

const golangCILockMessage = "parallel golangci-lint is running"

func transientPrePushFailure(out string) bool {
	return strings.Contains(out, golangCILockMessage)
}

// fixablePushRejection reports whether a rejected push looks like the
// repository's own verification refusing the commits, which is the one push
// failure a fix session can repair.
func fixablePushRejection(rejected *pushRejection) bool {
	return rejected != nil && strings.TrimSpace(rejected.out) != "" && rejected.local &&
		!transientPrePushFailure(rejected.out)
}

// hookConfigFiles are the files a fix session must not change to get a push
// through. Rewriting the verification that rejected the commits turns a red
// push green without fixing anything, and it is the shortest path out of the
// task, so the pipeline checks for it instead of only asking.
var hookConfigFiles = map[string]bool{
	"lefthook.yml":            true,
	"lefthook.yaml":           true,
	"lefthook-local.yml":      true,
	"lefthook-local.yaml":     true,
	"lefthook.toml":           true,
	".lefthook.yml":           true,
	".lefthook.yaml":          true,
	"Makefile":                true,
	".golangci.yml":           true,
	".golangci.yaml":          true,
	".golangci.toml":          true,
	".pre-commit-config.yml":  true,
	".pre-commit-config.yaml": true,
	RepoTestCmdPath:           true,
}

// hookConfigDirs are the hook directories with the same rule.
var hookConfigDirs = []string{".husky/", ".githooks/", ".hooks/"}

func touchesHookConfig(path string) bool {
	if hookConfigFiles[path] {
		return true
	}
	for _, dir := range hookConfigDirs {
		if strings.HasPrefix(path, dir) || strings.Contains(path, "/"+dir) {
			return true
		}
	}
	return false
}

// pushBranchWithFixes pushes, and hands a rejection from the repository's own
// pre-push verification to a fix session rather than ending the run there.
//
// Everything a repository checks only on push - type checks, unused exports,
// linters the local test gate does not run - otherwise surfaces after every
// review round is already spent, and the whole run is lost with nothing
// pushed. The session may repair code or install an ignored local dependency;
// the push itself stays with the pipeline, so its hook rerun remains the only
// thing that decides whether the branch really arrived.
func (r *run) pushBranchWithFixes() error {
	cache := deps.Cache{
		Root: r.o.DepsDir, Repo: r.o.Repo, Worktree: r.worktree,
	}
	var dependencyLinks []string
	if cache.Root != "" && cache.Repo != "" {
		dependencyLinks = cache.Link(func(err error) {
			r.rep.Warn(fmt.Sprintf("dependency cache lookup failed before push: %v", err))
		}, r.rep.Info)
	}
	defer func() { deps.Unlink(dependencyLinks) }()

	fixed := false
	noCommitFix := 0
	for attempt := 1; ; attempt++ {
		err := r.pushBranch()
		if err == nil {
			r.capturePushDependencies(cache, &dependencyLinks)
			if fixed {
				return r.flushFixComments()
			}
			return nil
		}
		var rejected *pushRejection
		if !errors.As(err, &rejected) || !fixablePushRejection(rejected) {
			return err
		}
		if noCommitFix != 0 {
			return fmt.Errorf("%w: push fix %d produced no commit and pre-push verification still fails; a human is needed: %s",
				ErrNoProgress, noCommitFix, r.targetReference())
		}
		if attempt > maxPushFixes {
			return err
		}
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}

		logPath := filepath.Join(r.logDir, pushLogFile)
		r.rep.Warn(fmt.Sprintf("push rejected before it reached the remote; push fix %d/%d, see %s",
			attempt, maxPushFixes, logPath))
		number := r.pushFixTotal + 1
		r.rep.Activity(fmt.Sprintf("preparing push fix %d", number), 0)

		preSHA, shaErr := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if shaErr != nil {
			return shaErr
		}
		remoteSHA, err := r.remoteHead()
		if err != nil {
			return err
		}
		hookStamp, err := r.hookStamp()
		if err != nil {
			return err
		}
		r.enter(PhaseFix)
		step, err := r.fixStep(fmt.Sprintf("push-fix-%d", number), pushFixPrompt(r.branch, tailLines(logPath, pushLogTailLines)))
		if err != nil {
			return err
		}
		afterSHA := step.head
		if afterSHA == preSHA {
			if err := r.requireHookUnchanged(hookStamp); err != nil {
				return err
			}
			if err := r.requireRemoteUnchanged(remoteSHA); err != nil {
				return err
			}
			if remoteSHA == afterSHA {
				r.headSHA = afterSHA
				if fixed {
					return r.flushFixComments()
				}
				return nil
			}
			noCommitFix = number
			r.rep.Info(fmt.Sprintf("push fix %d changed only local environment state; retrying pre-push verification", number))
			continue
		}
		if err := r.requireHookConfigUntouched(preSHA); err != nil {
			return err
		}
		if err := r.requireHookUnchanged(hookStamp); err != nil {
			return err
		}
		if err := r.requireRemoteUnchanged(remoteSHA); err != nil {
			return err
		}
		r.pushFixTotal = number
		fixed = true
		label := fmt.Sprintf("Push fix %d", number)
		r.recordRound(label, preSHA)
		r.queueFixComment(step, label, preSHA)

		// The repair is code no gate has seen yet.
		if err := r.ensureTestsGreen(); err != nil {
			return err
		}
		if err := r.requireHookConfigUntouched(preSHA); err != nil {
			return err
		}
		if err := r.requireHookUnchanged(hookStamp); err != nil {
			return err
		}
		if err := r.requireRemoteUnchanged(remoteSHA); err != nil {
			return err
		}
	}
}

func (r *run) capturePushDependencies(cache deps.Cache, links *[]string) {
	if cache.Root == "" || cache.Repo == "" {
		return
	}
	*links = append(*links, cache.Capture(func(err error) {
		r.rep.Warn(fmt.Sprintf("publishing dependency trees after push repair failed: %v", err))
	}, r.rep.Info)...)
}

func (r *run) remoteHead() (string, error) {
	if r.target.BranchOnly {
		sha, err := r.p.Git.LsRemote(r.ctx, r.o.RepoRoot, "origin", "refs/heads/"+r.branch)
		if errors.Is(err, git.ErrRefNotFound) {
			return "0000000000000000000000000000000000000000", nil
		}
		return sha, err
	}
	return r.p.GH.HeadSHA(r.ctx, r.o.RepoRoot, r.pr.Number)
}

func (r *run) hookStamp() (string, error) {
	sourcePath, err := r.p.Git.PrePushPath(r.ctx, r.worktree)
	if err != nil {
		return "", err
	}
	path, err := r.p.Git.WithHooksPath(r.hooksPath).PrePushPath(r.ctx, r.worktree)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return sourcePath + "|" + path + ":missing", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s|%s:%x", sourcePath, path, sha256.Sum256(data)), nil
}

func (r *run) requireHookUnchanged(expected string) error {
	actual, err := r.hookStamp()
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("the configured pre-push hook changed during a push repair; refusing to accept it: %s", r.targetReference())
	}
	return nil
}

func (r *run) requireRemoteUnchanged(expected string) error {
	actual, err := r.remoteHead()
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("the remote head changed from %s to %s during a push repair; refusing to accept an out-of-band push: %s", expected, actual, r.targetReference())
	}
	return nil
}

// requireHookConfigUntouched stops a run whose push fix silenced the
// verification instead of satisfying it.
func (r *run) requireHookConfigUntouched(preSHA string) error {
	changed, err := r.p.Git.ChangedFilesNoRenames(r.ctx, r.worktree, preSHA+"..HEAD")
	if err != nil {
		return err
	}
	hookPath, err := r.p.Git.PrePushPath(r.ctx, r.worktree)
	if err != nil {
		return err
	}
	hookPath, err = filepath.Rel(r.worktree, hookPath)
	if err != nil {
		return err
	}
	for _, path := range splitLines(changed) {
		if path == hookPath || touchesHookConfig(path) {
			return fmt.Errorf("the push fix changed %s; a rejected push is repaired in the code, not in the hook configuration: %s",
				path, r.targetReference())
		}
	}
	return nil
}

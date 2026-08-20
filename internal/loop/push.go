package loop

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxPushFixes bounds how often a rejected push is handed to a fix session:
// once for what the repository's verification reported, once for whatever that
// repair itself broke. After that the run stops the way it always did.
const maxPushFixes = 2

// pushLogTailLines bounds how much of the push output goes into a fix prompt.
// The full output stays in push-last.log.
const pushLogTailLines = 120

// pushRejection is a push that failed while the remote branch stayed where it
// was. It carries the command output, which is what a fix session needs: for a
// repository with pre-push verification that output is the whole complaint.
type pushRejection struct {
	branch  string
	sha     string
	out     string
	hookOut string
	local   bool
}

func (e *pushRejection) Error() string {
	return fmt.Sprintf("git push failed and origin/%s does not have %s:\n%s", e.branch, e.sha, e.out)
}

// fixablePushRejection reports whether a rejected push looks like the
// repository's own verification refusing the commits, which is the one push
// failure a fix session can repair.
func fixablePushRejection(rejected *pushRejection) bool {
	return rejected != nil && strings.TrimSpace(rejected.hookOut) != "" && rejected.local
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
	"package.json":            true,
	"Taskfile.yml":            true,
	"Taskfile.yaml":           true,
	"justfile":                true,
	"Justfile":                true,
	"pre-push.sh":             true,
	RepoTestCmdPath:           true,
}

// hookConfigDirs are the hook directories with the same rule.
var hookConfigDirs = []string{".husky/", ".githooks/", ".hooks/"}

// touchesHookConfig reports whether one changed path is hook configuration.
func touchesHookConfig(path string) bool {
	if path == RepoTestCmdPath {
		return true
	}
	if hookConfigFiles[filepath.Base(path)] {
		return true
	}
	for _, dir := range hookConfigDirs {
		if strings.HasPrefix(path, dir) || strings.Contains(path, "/"+dir) {
			return true
		}
	}
	if strings.HasSuffix(path, "/pre-push.sh") || strings.HasPrefix(path, "scripts/pre-push") {
		return true
	}
	return false
}

// pushBranchWithFixes pushes, and hands a rejection from the repository's own
// pre-push verification to a fix session rather than ending the run there.
//
// Everything a repository checks only on push - type checks, unused exports,
// linters the local test gate does not run - otherwise surfaces after every
// review round is already spent, and the whole run is lost with nothing
// pushed. The session repairs the finding and commits; the push itself stays
// with the pipeline, so pushBranch's barrier remains the only thing that
// decides whether the branch really arrived.
func (r *run) pushBranchWithFixes() error {
	fixed := false
	for attempt := 1; ; attempt++ {
		err := r.pushBranch()
		if err == nil {
			if fixed {
				return r.flushFixComments()
			}
			return nil
		}
		var rejected *pushRejection
		if !errors.As(err, &rejected) || attempt > maxPushFixes || !fixablePushRejection(rejected) {
			return err
		}
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}

		logPath := filepath.Join(r.logDir, "push-last.log")
		r.rep.Warn(fmt.Sprintf("the push was rejected before it reached the remote; starting push fix %d/%d, see %s",
			attempt, maxPushFixes, logPath))

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
		number := r.pushFixTotal + 1
		tag := fmt.Sprintf("push-fix-%d", number)
		r.enter(PhaseFix)
		if err := r.codexCall(tag, pushFixPrompt(r.branch, tailLines(logPath, pushLogTailLines))); err != nil {
			return err
		}
		if err := r.questionGate(tag); err != nil {
			return err
		}
		if err := r.ensureCommitted(tag); err != nil {
			return err
		}
		afterSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if err != nil {
			return err
		}
		if afterSHA == preSHA {
			return fmt.Errorf("%w: push fix %d produced no commit, a human is needed: %s",
				ErrNoProgress, number, r.targetReference())
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
		r.queueFixComment(tag, label, preSHA)

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

func (r *run) remoteHead() (string, error) {
	if r.target.BranchOnly {
		sha, err := r.p.Git.LsRemote(r.ctx, r.o.RepoRoot, "origin", "refs/heads/"+r.branch)
		if err != nil && strings.Contains(err.Error(), "could not resolve refs/heads/") {
			return "0000000000000000000000000000000000000000", nil
		}
		return sha, err
	}
	return r.p.GH.HeadSHA(r.ctx, r.o.RepoRoot, r.pr.Number)
}

func (r *run) hookStamp() (string, error) {
	path, err := r.p.Git.PrePushPath(r.ctx, r.worktree)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return path + ":missing", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%x", path, sha256.Sum256(data)), nil
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
	changed, err := r.p.Git.ChangedFiles(r.ctx, r.worktree, preSHA+"..HEAD")
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

package loop

import (
	"errors"
	"fmt"
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
	branch string
	sha    string
	out    string
}

func (e *pushRejection) Error() string {
	return fmt.Sprintf("git push failed and origin/%s does not have %s:\n%s", e.branch, e.sha, e.out)
}

// unfixablePushMarkers are git's own words for the push failures no fix
// session may touch: someone else moved the branch, credentials, a protected
// branch, the network.
var unfixablePushMarkers = []string{
	"non-fast-forward",
	"fetch first",
	"Updates were rejected",
	"Authentication failed",
	"could not read Username",
	"could not read Password",
	"Permission denied",
	"Permission to",
	"protected branch",
	"GH006",
	"shallow update not allowed",
	"Could not resolve host",
	"Connection timed out",
	"Connection refused",
	"The remote end hung up",
}

// fixablePushRejection reports whether a rejected push looks like the
// repository's own verification refusing the commits, which is the one push
// failure a fix session can repair.
//
// The test is negative on purpose. There is no portable signal that says "a
// pre-push hook rejected this": hook frameworks print whatever they like, and
// git reports their exit code as the same failed push as everything else. The
// failures that must never reach a session all announce themselves in git's
// wording, so those are what is matched.
func fixablePushRejection(out string) bool {
	for _, marker := range unfixablePushMarkers {
		if strings.Contains(out, marker) {
			return false
		}
	}
	return strings.TrimSpace(out) != ""
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
	".lefthook.yml":           true,
	".lefthook.yaml":          true,
	".pre-commit-config.yaml": true,
}

// hookConfigDirs are the hook directories with the same rule.
var hookConfigDirs = []string{".husky/", ".githooks/", ".hooks/"}

// touchesHookConfig reports whether one changed path is hook configuration.
func touchesHookConfig(path string) bool {
	if hookConfigFiles[filepath.Base(path)] {
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
// pushed. The session repairs the finding and commits; the push itself stays
// with the pipeline, so pushBranch's barrier remains the only thing that
// decides whether the branch really arrived.
//
// Known ceiling: a session that pushes on its own with the verification
// disabled would satisfy that barrier. The prompt forbids it and the hook
// configuration is checked afterwards, but a bypass that leaves no trace in
// the diff is not detectable here.
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
		if !errors.As(err, &rejected) || attempt > maxPushFixes || !fixablePushRejection(rejected.out) {
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

		r.pushFixTotal = number
		fixed = true
		label := fmt.Sprintf("Push fix %d", number)
		r.recordRound(label, preSHA)
		r.queueFixComment(tag, label, preSHA)

		// The repair is code no gate has seen yet.
		if err := r.ensureTestsGreen(); err != nil {
			return err
		}
	}
}

// requireHookConfigUntouched stops a run whose push fix silenced the
// verification instead of satisfying it.
func (r *run) requireHookConfigUntouched(preSHA string) error {
	changed, err := r.p.Git.ChangedFiles(r.ctx, r.worktree, preSHA+"..HEAD")
	if err != nil {
		return err
	}
	for _, path := range splitLines(changed) {
		if touchesHookConfig(path) {
			return fmt.Errorf("the push fix changed %s; a rejected push is repaired in the code, not in the hook configuration: %s",
				path, r.targetReference())
		}
	}
	return nil
}

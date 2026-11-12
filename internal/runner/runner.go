package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	macnotify "github.com/yungweng/quorum/internal/notify"
	"github.com/yungweng/quorum/internal/rundir"

	"github.com/yungweng/quorum/internal/agentstate"
	"github.com/yungweng/quorum/internal/automerge"
	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/prkey"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/usagelimit"
)

// Runner performs one automated review from start to finish.
type Runner struct {
	Cfg config.Config
	P   paths.P
	Log *logbook.Logger
	// TerminalNotify is set by commands that run in a terminal. It keeps their
	// notifications on the same OSC 777 path as review and babysit instead of
	// presenting them as notifications from an unrelated macOS app.
	TerminalNotify func(title, body string)

	GH    *gh.Client
	Git   git.G
	Tools Tools
}

// defaultUsageLimitPause covers a refusal whose reset time could not be
// parsed: long enough to stop the hammering, short enough to notice when the
// account comes back early.
const defaultUsageLimitPause = time.Hour

// Review clones or refreshes the repository, reviews the pull request and
// records the outcome. It runs in the detached child process that owns the
// marker.
func (r *Runner) Review(ctx context.Context, key, repo string, number int, sha, title, author, reqAt string) error {
	r.applyPriority()
	reqAt = r.requestAnswered(key, reqAt)

	clone, err := r.ensureClone(ctx, repo)
	if err != nil {
		r.Log.Printf("%s: clone/fetch failed: %v", key, err)
		r.recordFailure(key, sha, "", fmt.Sprintf("clone or fetch failed: %v", err), "", false)
		return err
	}

	runLog := rundir.ForPR(r.P.RunLogDir, repo, number) + ".log"
	if err := os.MkdirAll(filepath.Dir(runLog), 0o755); err != nil {
		return err
	}

	// The resume candidate must be read before Mark(Running) resets RunDir.
	resumeDir := r.resumableRunDir(key, sha)

	kind := history.KindReview
	if r.Cfg.AgentAction == config.ActionBabysit {
		kind = history.KindBabysit
	}
	r.Log.Printf("%s: reviewing %s (%s)", key, short(sha), title)
	r.mutate(key, func(rec *state.Record) {
		rec.Title = title
		rec.Author = author
		rec.Mark(state.Running, "")
		rec.SHA = sha
		rec.RunLog = runLog
		rec.RunDir = ""
		// An orphaned run recovered later must never look resumable.
		rec.ResumableRun = false
		rec.StartedAt = state.Now()
		rec.Kind = kind
		rec.Rounds, rec.Converged = 0, false
	})

	var (
		res    loop.Result
		runErr error
	)
	switch r.Cfg.AgentAction {
	case config.ActionBabysit:
		res, runErr = r.babysit(ctx, clone, repo, number, resumeDir, runLog)
	default:
		res.LastFindings, res.RunDir, runErr = r.reviewOnce(ctx, key, clone, repo, number, resumeDir, runLog)
	}
	findings, runDir := res.LastFindings, res.RunDir
	if kind == history.KindBabysit {
		// Written while the record still says running, so whichever outcome is
		// recorded below carries it into the history log.
		r.mutate(key, func(rec *state.Record) {
			rec.Rounds = state.Num(res.Rounds)
			rec.Converged = res.Converged
		})
	}

	if runErr == nil {
		mergeStatus := ""
		eligible := findings.MergeEligible()
		// Only a babysit run's loop result carries the pull request. A review
		// run leaves it zero, and the merge path refuses a draft itself.
		merge, skip := automerge.Decide(r.Cfg.AutoMerge, r.Cfg.Post, eligible, res.PR.IsDraft, res.SuggestionCommits)
		if skip != "" {
			r.Log.Printf("%s: auto-merge skipped, %s", key, skip)
		} else if merge {
			mergeResult, mergeErr := automerge.Complete(ctx, r.GH, clone, repo, number, findings.HeadSHA,
				r.Cfg.AutoMergeAuthors, r.Cfg.AutoMergeTimeout, func() {
					r.Log.Printf("%s: waiting for required checks before merge", key)
					r.recordAutoMergePending(key, runDir, findings)
				})
			mergeStatus = mergeResult.Status
			if mergeErr != nil {
				reason := mergeErr.Error()
				r.Log.Printf("%s: FAILED, %s", key, reason)
				r.recordAutoMergeFailure(key, reqAt, runDir, findings, reason)
				r.notify(fmt.Sprintf("Auto-merge failed: %s#%d", prkey.Name(repo), number), reason, urlOf(findings))
				return fmt.Errorf("%s: %s", key, reason)
			}
		}
		posted := urlOf(findings)
		if posted == "" {
			posted = "not posted"
		}
		r.Log.Printf("%s: done, %s -> %s", key, findings.Summary(), posted)
		r.mutate(key, func(rec *state.Record) {
			rec.Mark(state.OK, "")
			rec.SHA = sha
			rec.RunDir = runDir
			rec.CommentURL = urlOf(findings)
			setCounts(rec, findings)
			rec.Fails = 0
			rec.ResumableRun = false
			// Recording the request timestamp is what marks it as handled.
			markHandled(rec, reqAt)
		})
		note := summaryLine(findings)
		switch mergeStatus {
		case automerge.Merged:
			note += "; merged"
		case automerge.Queued:
			note += "; queued for merge"
		case automerge.ApprovalRequired:
			note += "; awaiting approval"
		}
		switch {
		case mergeStatus == automerge.ApprovalRequired:
			r.notifyAction(macnotify.ApprovalRequired(repo, number))
		case automerge.ReadyNoticeDue(r.Cfg.NotifyReadyToMerge, mergeStatus, eligible, res.PR.IsDraft, res.SuggestionCommits):
			r.notifyAction(macnotify.ReadyToMerge(repo, number))
		default:
			r.notify(fmt.Sprintf("Reviewed %s#%d", prkey.Name(repo), number), note, urlOf(findings))
		}
		return nil
	}

	var limit *usagelimit.Error
	if errors.As(runErr, &limit) {
		until := limit.ResetAt
		if until.IsZero() || !until.After(time.Now()) {
			until = time.Now().Add(defaultUsageLimitPause)
		}
		wrote, pauseErr := agentstate.WritePause(r.P.StateDir, until)
		if pauseErr != nil {
			r.Log.Printf("%s: could not record the usage-limit pause: %v", key, pauseErr)
		}
		r.Log.Printf("%s: usage limit hit, pausing new reviews until %s", key, until.Format(time.RFC3339))
		r.recordUsageLimit(key, sha, recordableRunDir(runDir, runErr), runLog, until, runErr)
		if wrote {
			r.notify("Usage limit hit", "Pausing new reviews until "+until.Format(time.RFC1123), "")
		}
		return fmt.Errorf("%s: usage limit, paused until %s", key, until.Format(time.RFC3339))
	}

	reason := runErr.Error()
	if res.Divergence != nil && res.Divergence.Verdict != "" {
		r.Log.Printf("%s: STOPPED, divergence analysis = %s (log: %s)", key, res.Divergence.Verdict, runLog)
		recordedSHA := findings.HeadSHA
		if recordedSHA == "" {
			recordedSHA = sha
		}
		r.recordDivergenceResult(key, recordedSHA, reqAt, runDir, runLog, res.DivergenceCommentURL, findings, reason)
		r.notify(fmt.Sprintf("Review loop stopped: %s#%d", prkey.Name(repo), number),
			"divergence analysis: "+res.Divergence.Verdict, res.DivergenceCommentURL)
		return fmt.Errorf("%s: %s", key, reason)
	}
	r.Log.Printf("%s: FAILED, %s (log: %s)", key, reason, runLog)
	// The request timestamp stays unrecorded on purpose, so the next poll
	// retries until MaxRetries is reached. An aggregator or verifier failure
	// leaves the completed reviewer outputs reusable; that retry resumes them
	// instead of paying for a fresh fan-out.
	_, resumable := review.ResumableRunDir(runErr)
	r.recordFailure(key, sha, recordableRunDir(runDir, runErr), reason, runLog, resumable)
	r.notify(fmt.Sprintf("Review failed: %s#%d", prkey.Name(repo), number), reason, "")
	return fmt.Errorf("%s: %s", key, reason)
}

// markHandled records which review request a finished run answered. Without
// one it leaves the recorded request alone: storing the empty timestamp of a
// `quorum run` made every request the agent had already handled look new
// again on the next poll.
func markHandled(rec *state.Record, reqAt string) {
	if reqAt != "" {
		rec.ReqAt = reqAt
	}
}

// requestAnswered is the review request a run answers. The poll passes the
// one that started the run. `quorum run` has none and answers the newest the
// poll had seen when the run began; one that arrives while it runs is not
// covered by this review and stays open.
func (r *Runner) requestAnswered(key, reqAt string) string {
	if reqAt != "" {
		return reqAt
	}
	st, err := state.Read(r.P.StateFile)
	if err != nil {
		return ""
	}
	return st.PRs[key].SeenReqAt
}

func (r *Runner) recordDivergenceResult(key, sha, reqAt, runDir, runLog, reportURL string,
	findings review.Findings, reason string,
) {
	r.mutate(key, func(rec *state.Record) {
		rec.Mark(state.Failed, reason)
		rec.SHA = sha
		rec.RunDir = runDir
		rec.RunLog = runLog
		rec.CommentURL = reportURL
		if rec.CommentURL == "" {
			rec.CommentURL = urlOf(findings)
		}
		setCounts(rec, findings)
		rec.Fails = 0
		rec.ResumableRun = false
		// Phase 1 is report-only. Mark this request handled so the agent
		// cannot turn it into an unbounded sequence of fresh runs.
		markHandled(rec, reqAt)
	})
}

// recordAutoMergePending preserves the completed review while the same
// detached run waits for protected-branch checks. ReqAt remains unset until
// the merge either succeeds or reaches a terminal failure.
func (r *Runner) recordAutoMergePending(key, runDir string, findings review.Findings) {
	r.mutate(key, func(rec *state.Record) {
		rec.Mark(state.Running, "waiting for required checks before merge")
		rec.SHA = findings.HeadSHA
		rec.RunDir = runDir
		rec.CommentURL = urlOf(findings)
		setCounts(rec, findings)
	})
}

// recordAutoMergeFailure preserves the successful review and records the
// failed final action as its reason. ReqAt marks this request handled, while
// resetting Fails keeps merge errors out of the review retry budget.
func (r *Runner) recordAutoMergeFailure(key, reqAt, runDir string, findings review.Findings, reason string) {
	r.mutate(key, func(rec *state.Record) {
		rec.Mark(state.OK, reason)
		rec.SHA = findings.HeadSHA
		rec.RunDir = runDir
		rec.CommentURL = urlOf(findings)
		setCounts(rec, findings)
		markHandled(rec, reqAt)
		rec.Fails = 0
		rec.ResumableRun = false
	})
}

// reviewOnce posts one review and stops. A resumeDir from a prior failed
// attempt is tried first; when the resume itself turns out unusable, one
// fresh run replaces it, and only then.
func (r *Runner) reviewOnce(ctx context.Context, key, clone, repo string, number int, resumeDir, runLog string) (review.Findings, string, error) {
	logFile, err := os.Create(runLog)
	if err != nil {
		return review.Findings{}, "", err
	}
	defer logFile.Close()

	rev := &review.Runner{
		GH:  r.GH,
		Git: r.Git,
		// Recording the run directory as soon as it exists lets the dashboard
		// follow the reviewers while the review is still going.
		Rep: &logReporter{w: logFile, onRunDir: func(dir string) {
			r.mutate(key, func(rec *state.Record) { rec.RunDir = dir })
		}},
	}
	opts := r.reviewOptions(repo, number, clone)
	opts.ResumeRun = resumeDir
	if resumeDir != "" {
		r.Log.Printf("%s: resuming completed reviewer output from %s", key, resumeDir)
	}
	res, err := rev.RunWithFallback(ctx, opts, func(err error) {
		r.Log.Printf("%s: resume of %s failed (%v), starting a fresh run", key, resumeDir, err)
	})
	if err != nil {
		return review.Findings{}, review.RunDirOf(err), err
	}
	return res.Findings, res.RunDir, nil
}

// recordableRunDir is the directory a failure record should carry so a later
// poll can resume it. A babysit failure reports the pipeline's own root,
// which holds no reviewer output; the review round's directory travels in
// the error chain and takes precedence when it is there.
func recordableRunDir(runDir string, runErr error) string {
	if reviewDir := review.RunDirOf(runErr); reviewDir != "" {
		return reviewDir
	}
	return runDir
}

// resumableRunDir returns the prior run to resume for this pull request's
// head, or "" when nothing qualifies: no record, not marked resumable, a
// different head (the reviewer output would describe code that no longer
// exists), or an output directory with no reviewer output left in it. The
// cache collector may also have taken the directory in the meantime; that
// race is accepted because a vanished directory fails as ErrResumeUnusable
// and falls back to a fresh run.
func (r *Runner) resumableRunDir(key, sha string) string {
	st, err := state.Read(r.P.StateFile)
	if err != nil {
		return ""
	}
	rec, ok := st.PRs[key]
	if !ok || !rec.ResumableRun || rec.RunDir == "" || rec.SHA != sha {
		return ""
	}
	if !review.HasReviewerOutput(rec.RunDir) {
		return ""
	}
	return rec.RunDir
}

// recordUsageLimit marks a review paused by the engine's usage limit. Fails
// and ReqAt stay untouched: the refusal says nothing about this pull request,
// so it must not count toward MaxRetries or mark the request handled. The
// first poll after the pause reviews it again, resuming the run directory
// when its reviewer outputs survived.
func (r *Runner) recordUsageLimit(key, sha, runDir, runLog string, until time.Time, runErr error) {
	// Unlike other failures, a usage limit leaves any run directory
	// resumable: the refusal can hit in any phase, reviewers included, and
	// says nothing about the output that completed before it.
	resumable := runDir != "" && review.RunDirOf(runErr) != ""
	r.mutate(key, func(rec *state.Record) {
		rec.Mark(state.Deferred, "usage limit, paused until "+until.Format(time.RFC3339))
		rec.SHA = sha
		if runDir != "" {
			rec.RunDir = runDir
		}
		if runLog != "" {
			rec.RunLog = runLog
		}
		rec.ResumableRun = resumable
	})
}

func (r *Runner) babysit(ctx context.Context, clone, repo string, number int, resumeDir, runLog string) (loop.Result, error) {
	logFile, err := os.Create(runLog)
	if err != nil {
		return loop.Result{}, err
	}
	defer logFile.Close()
	testCmd := ""
	testCmdSet := false
	if r.Cfg.LoopMode != config.LoopOnline {
		var err error
		testCmd, testCmdSet, err = config.RepoTestCmd(r.P.TestCmdDir, repo)
		if err != nil {
			return loop.Result{}, fmt.Errorf("reading the test command for %s: %w", repo, err)
		}
	}

	rev := &review.Runner{GH: r.GH, Git: r.Git, Rep: review.NopReporter{}}
	pipe := &loop.Pipeline{
		GH:     r.GH,
		Git:    r.Git,
		Review: rev,
		Rep:    &loopLogReporter{w: logFile},
		// No terminal behind a launchd agent, so interactive gates must abort
		// rather than wait for input that can never arrive.
		In: nil,
	}
	o := LoopOptions(r.Cfg, r.P, r.Tools, repo, clone, number)
	o.Rules = r.repoRules(repo)
	// A draft only reaches the runner when SKIP_DRAFTS=0, which is the
	// explicit opt-in the manual --draft flag provides in a terminal.
	o.AllowDraft = !r.Cfg.SkipDrafts
	o.TestCmd = testCmd
	o.TestCmdSet = testCmdSet
	o.ResumeRun = resumeDir
	o.Interactive = false
	res, err := pipe.Run(ctx, o)
	if res == nil {
		return loop.Result{}, err
	}
	return *res, err
}

// repoRules loads the user-local review rules for repo. The unattended agent
// only warns on a read failure: nobody is present to fix a permission error,
// and the full general review still runs; the manual commands fail instead.
func (r *Runner) repoRules(repo string) string {
	rules, err := config.RepoRules(r.P.RulesDir, repo)
	if err != nil {
		r.Log.Printf("%s: reading the review rules failed, continuing without them: %v", repo, err)
	}
	return rules
}

func (r *Runner) reviewOptions(repo string, number int, clone string) review.Options {
	o := ReviewOptions(r.Cfg, r.P, r.Tools, repo, clone, number)
	o.Rules = r.repoRules(repo)
	// Never passed for an automated run: allowing an .envrc the PR itself
	// changed means executing a stranger's code unattended.
	o.AllowEnvrcChange = false
	return o
}

func (r *Runner) mutate(key string, fn func(*state.Record)) {
	stateErr, historyErr := history.Record(r.P.StateFile, r.P.HistoryFile, key, history.SourceAgent, fn)
	if stateErr != nil {
		r.Log.Printf("%s: could not record the result: %v", key, stateErr)
	}
	if historyErr != nil {
		r.Log.Printf("%s: could not add to the history log: %v", key, historyErr)
	}
}

// applyPriority lowers this process's scheduling priority. Everything it spawns
// inherits it, so the Codex reviewers stop competing with whatever the user is
// actually doing.
func (r *Runner) applyPriority() {
	if r.Cfg.Nice <= 0 {
		return
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, os.Getpid(), r.Cfg.Nice); err != nil {
		r.Log.Printf("could not set priority to %d: %v", r.Cfg.Nice, err)
	}
}

func (r *Runner) recordFailure(key, sha, runDir, reason, runLog string, resumable bool) {
	r.mutate(key, func(rec *state.Record) {
		rec.Mark(state.Failed, reason)
		rec.SHA = sha
		rec.Fails++
		if runDir != "" {
			rec.RunDir = runDir
		}
		if runLog != "" {
			rec.RunLog = runLog
		}
		rec.ResumableRun = resumable && rec.RunDir != ""
	})
}

// ensureClone keeps one clone per repository and refreshes it. Concurrent
// reviews of the same repository must not fetch into it at the same time, so
// the directory is locked for the duration.
func (r *Runner) ensureClone(ctx context.Context, repo string) (string, error) {
	dir := filepath.Join(r.P.CloneDir, filepath.FromSlash(repo))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	unlock, err := proc.LockFile(dir + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()

	// Both go through the wrappers rather than the binaries: a cancelled run
	// then reaches the transport git spawned, and the clone gets the same
	// retry policy as every other GitHub call.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if err := r.Git.FetchAll(ctx, dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	if err := r.GH.Clone(ctx, repo, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func setCounts(rec *state.Record, f review.Findings) {
	rec.Blockers = state.Num(f.Blockers)
	rec.Critical = state.Num(f.Critical)
	rec.Suggestions = state.Num(f.Suggestions)
	rec.Questions = state.Num(f.Questions)
}

func summaryLine(f review.Findings) string {
	if f.Blockers == 0 && f.Critical == 0 && f.Suggestions == 0 && f.Questions == 0 {
		return "nothing found"
	}
	return fmt.Sprintf("%d blockers, %d critical, %d suggestions", f.Blockers, f.Critical, f.Suggestions)
}

func urlOf(f review.Findings) string {
	if f.CommentURL == nil {
		return ""
	}
	return *f.CommentURL
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

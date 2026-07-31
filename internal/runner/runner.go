package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/automerge"
	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/state"
)

// Runner performs one automated review from start to finish.
//
// It used to start pr-codex-review as a child process and then reconstruct what
// had happened by scanning its stdout for a "Run dir" line and reading the JSON
// it left behind. The review core is a package now, so the outcome is simply
// the return value and the failure modes are typed errors.
type Runner struct {
	Cfg config.Config
	P   paths.P
	Log *logbook.Logger

	GH     *gh.Client
	Git    git.G
	GitBin string
	GHBin  string

	CodexBin  string
	DirenvBin string
}

// Keep this aligned with the fix loop's no-check registration window.
const mergeNoChecksGrace = 180 * time.Second

// Review clones or refreshes the repository, reviews the pull request and
// records the outcome. It runs in the detached child process that owns the
// marker.
func (r *Runner) Review(ctx context.Context, key, repo string, number int, sha, title, author, reqAt string) error {
	r.applyPriority()

	clone, err := r.ensureClone(ctx, repo)
	if err != nil {
		r.Log.Printf("%s: clone/fetch failed: %v", key, err)
		r.recordFailure(key, sha, "", fmt.Sprintf("clone or fetch failed: %v", err))
		return err
	}

	stamp := time.Now().Format("20060102-150405")
	runLog := filepath.Join(r.P.RunLogDir, fmt.Sprintf("%s-pr-%d-%s.log",
		strings.ReplaceAll(repo, "/", "-"), number, stamp))
	if err := os.MkdirAll(filepath.Dir(runLog), 0o755); err != nil {
		return err
	}

	r.Log.Printf("%s: reviewing %s (%s)", key, short(sha), title)
	r.mutate(key, func(rec *state.Record) {
		rec.Title = title
		rec.Author = author
		rec.Mark(state.Running, "")
		rec.SHA = sha
		rec.RunLog = runLog
		rec.RunDir = ""
		rec.StartedAt = state.Now()
	})

	action := r.Cfg.AgentAction
	if action == "" {
		action = config.ActionReview
	}

	var (
		findings review.Findings
		runDir   string
		runErr   error
	)
	switch action {
	case config.ActionBabysit:
		findings, runDir, runErr = r.babysit(ctx, key, clone, repo, number, runLog)
	default:
		findings, runDir, runErr = r.reviewOnce(ctx, key, clone, repo, number, runLog)
	}

	if runErr == nil {
		mergeStatus := ""
		if automerge.Allowed(r.Cfg.AutoMergeAgent, r.Cfg.Post, findings) {
			mergeResult, mergeErr := automerge.Run(ctx, r.GH, repo, number, findings.HeadSHA)
			if errors.Is(mergeErr, automerge.ErrMergeNotReady) {
				r.Log.Printf("%s: waiting for required checks before merge", key)
				r.recordAutoMergePending(key, runDir, findings)
				retryResult, retryErr := r.retryAutoMergeAfterChecks(ctx, clone, repo, number, findings.HeadSHA)
				if mergeResult.ApprovalAttempted {
					retryResult.ApprovalAttempted = true
				}
				if mergeResult.ApprovalCreated {
					retryResult.ApprovalCreated = true
				}
				mergeResult, mergeErr = retryResult, retryErr
			}
			mergeStatus = mergeResult.Status
			if mergeErr != nil {
				reason := fmt.Sprintf("auto-merge failed: %v", mergeErr)
				if mergeResult.ApprovalCreated {
					reason = fmt.Sprintf("auto-merge failed after the approval was posted: %v", mergeErr)
				} else if mergeResult.ApprovalAttempted {
					reason = fmt.Sprintf("auto-merge failed and GitHub's approval result is unknown: %v", mergeErr)
				}
				r.Log.Printf("%s: FAILED, %s", key, reason)
				r.recordAutoMergeFailure(key, reqAt, runDir, findings, reason)
				r.notify(fmt.Sprintf("Auto-merge failed: %s#%d", nameOf(repo), number), reason, urlOf(findings))
				return fmt.Errorf("%s: %s", key, reason)
			}
		}
		r.Log.Printf("%s: done, %s -> %s", key, findings.Summary(), orDash(urlOf(findings)))
		r.mutate(key, func(rec *state.Record) {
			rec.Mark(state.OK, "")
			rec.SHA = sha
			rec.RunDir = runDir
			rec.CommentURL = urlOf(findings)
			rec.Blockers = state.Num(findings.Blockers)
			rec.Critical = state.Num(findings.Critical)
			rec.Suggestions = state.Num(findings.Suggestions)
			rec.Questions = state.Num(findings.Questions)
			rec.Fails = 0
			// Recording the request timestamp is what marks it as handled.
			rec.ReqAt = reqAt
		})
		note := summaryLine(findings)
		switch mergeStatus {
		case automerge.Merged:
			note += "; merged"
		}
		r.notify(fmt.Sprintf("Reviewed %s#%d", nameOf(repo), number), note, urlOf(findings))
		return nil
	}

	reason := runErr.Error()
	r.Log.Printf("%s: FAILED, %s (log: %s)", key, reason, runLog)
	// The request timestamp stays unrecorded on purpose, so the next poll
	// retries until MaxRetries is reached.
	r.recordFailureWith(key, sha, runDir, reason, runLog)
	r.notify(fmt.Sprintf("Review failed: %s#%d", nameOf(repo), number), reason, "")
	return fmt.Errorf("%s: %s", key, reason)
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
		rec.Blockers = state.Num(findings.Blockers)
		rec.Critical = state.Num(findings.Critical)
		rec.Suggestions = state.Num(findings.Suggestions)
		rec.Questions = state.Num(findings.Questions)
	})
}

func (r *Runner) retryAutoMergeAfterChecks(ctx context.Context, clone, repo string, number int, sha string) (automerge.Result, error) {
	return r.retryAutoMergeAfterChecksWithin(ctx, clone, repo, number, sha, mergeNoChecksGrace)
}

func (r *Runner) retryAutoMergeAfterChecksWithin(ctx context.Context, clone, repo string, number int, sha string, noChecksGrace time.Duration) (automerge.Result, error) {
	var noChecksDeadline time.Time
	for {
		checkState, output, err := r.GH.WatchChecks(ctx, clone, number)
		if err != nil {
			return automerge.Result{}, fmt.Errorf("waiting for required checks: %w", err)
		}
		switch checkState {
		case gh.ChecksPass:
			return automerge.Run(ctx, r.GH, repo, number, sha)
		case gh.ChecksFail:
			return automerge.Result{}, fmt.Errorf("required checks failed: %s", firstLine(output))
		case gh.ChecksNone, gh.ChecksPending:
			delay := 10 * time.Second
			if checkState == gh.ChecksNone {
				if noChecksDeadline.IsZero() {
					noChecksDeadline = time.Now().Add(noChecksGrace)
				}
				if !time.Now().Before(noChecksDeadline) {
					return automerge.Run(ctx, r.GH, repo, number, sha)
				}
				delay = min(20*time.Second, time.Until(noChecksDeadline))
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return automerge.Result{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
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
		rec.Blockers = state.Num(findings.Blockers)
		rec.Critical = state.Num(findings.Critical)
		rec.Suggestions = state.Num(findings.Suggestions)
		rec.Questions = state.Num(findings.Questions)
		rec.ReqAt = reqAt
		rec.Fails = 0
	})
}

// reviewOnce posts one review and stops.
func (r *Runner) reviewOnce(ctx context.Context, key, clone, repo string, number int, runLog string) (review.Findings, string, error) {
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
	res, err := rev.Run(ctx, r.reviewOptions(repo, number, clone))
	if err != nil {
		return review.Findings{}, "", err
	}
	return res.Findings, res.RunDir, nil
}

// babysit runs the full fix loop instead of a single review.
//
// This is what the three separate tools could not do: the daemon knew how to
// start a review but had no way to reach the fix pipeline, because that was a
// different binary with its own idea of where things live.
func (r *Runner) babysit(ctx context.Context, key, clone, repo string, number int, runLog string) (review.Findings, string, error) {
	logFile, err := os.Create(runLog)
	if err != nil {
		return review.Findings{}, "", err
	}
	defer logFile.Close()

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
	res, err := pipe.Run(ctx, loop.Options{
		Repo:          repo,
		RepoRoot:      clone,
		Number:        number,
		Model:         r.Cfg.FixModel,
		Effort:        r.Cfg.FixEffort,
		Reviewers:     r.Cfg.Reviewers,
		ReviewModel:   r.Cfg.ReviewModel,
		ReviewEffort:  r.Cfg.ReviewEffort,
		MaxIter:       r.Cfg.MaxIter,
		MaxCIFixes:    r.Cfg.MaxCIFixes,
		FixTimeout:    r.Cfg.FixTimeout,
		Bypass:        !r.Cfg.Sandboxed,
		Interactive:   false,
		UseDirenv:     true,
		RunsDir:       r.P.BabysitRuns,
		ReviewRunsDir: r.P.ReviewRuns,
		DepsDir:       r.P.DepsCache,
		CodexBin:      r.CodexBin,
		DirenvBin:     r.DirenvBin,
	})
	if res == nil {
		return review.Findings{}, "", err
	}
	return res.LastFindings, res.RunDir, err
}

// reviewOptions maps the daemon's configuration onto a review run.
func (r *Runner) reviewOptions(repo string, number int, clone string) review.Options {
	return review.Options{
		Repo:     repo,
		Number:   number,
		RepoRoot: clone,
		// No concurrency limit: every pass runs at once, which is the point. A
		// review that trickles through its passes is a review you wait for.
		Runs:          r.Cfg.Reviewers,
		Model:         r.Cfg.ReviewModel,
		Effort:        r.Cfg.ReviewEffort,
		ReviewTimeout: r.Cfg.ReviewTimeout,
		Post:          r.Cfg.Post,
		UseDirenv:     true,
		// Never passed for an automated run: allowing an .envrc the PR itself
		// changed means executing a stranger's code unattended.
		AllowEnvrcChange: false,
		RunsDir:          r.P.ReviewRuns,
		SharedRunsDirs:   []string{r.P.BabysitRuns},
		DepsDir:          r.P.DepsCache,
		CodexBin:         r.CodexBin,
		DirenvBin:        r.DirenvBin,
	}
}

// mutate updates the state file and complains loudly if it cannot. Losing this
// write means losing the outcome of a review that has already been paid for.
//
// It is also where the agent's runs reach the history log. Every terminal
// state the agent reaches passes through here, so hooking it is what makes it
// impossible to add an outcome later that quietly never gets logged.
func (r *Runner) mutate(key string, fn func(*state.Record)) {
	var before, after state.Record
	capture := func(rec *state.Record) {
		before = *rec
		fn(rec)
		after = *rec
	}
	if err := state.Mutate(r.P.StateFile, key, capture); err != nil {
		r.Log.Printf("%s: could not record the result: %v", key, err)
		return
	}
	if run, ok := history.FromState(key, before, after, history.SourceAgent); ok {
		if err := history.Append(r.P.HistoryFile, run); err != nil {
			// The run itself happened and its output is on disk, so a log that
			// could not be extended is worth a line, not a failure.
			r.Log.Printf("%s: could not add to the history log: %v", key, err)
		}
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

func (r *Runner) recordFailure(key, sha, runDir, reason string) {
	r.recordFailureWith(key, sha, runDir, reason, "")
}

func (r *Runner) recordFailureWith(key, sha, runDir, reason, runLog string) {
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
	unlock, err := lockPath(dir + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, r.GitBin, "-C", dir, "fetch", "--prune", "--quiet", "origin")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git fetch: %s", firstLine(string(out)))
		}
		return dir, nil
	}
	cmd := exec.CommandContext(ctx, r.GHBin, "repo", "clone", repo, dir, "--", "--quiet")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("gh repo clone: %s", firstLine(string(out)))
	}
	return dir, nil
}

// lockPath takes an exclusive lock on a lock file and returns the release.
func lockPath(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Progress is what a running review reports about itself.
type Progress struct {
	Done      int
	Failed    int
	Requested int
}

// ReadProgress counts finished reviewers of a run in flight, from the events
// log the run writes as it goes. A run directory that is not there yet simply
// reports nothing.
//
// The run opens the log with a "start" line when it launches the reviewers, so
// a zero count means the reviewers are running and none has finished yet,
// rather than the run still preparing its worktree. Only reported outcomes are
// counted; any other line is ignored.
func ReadProgress(runDir string, requested int) (Progress, bool) {
	if runDir == "" {
		return Progress{}, false
	}
	f, err := os.Open(filepath.Join(runDir, "output", "events.log"))
	if err != nil {
		return Progress{}, false
	}
	defer f.Close()
	p := Progress{Requested: requested}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		switch {
		case strings.HasPrefix(sc.Text(), "ok "):
			p.Done++
		case strings.HasPrefix(sc.Text(), "fail "):
			p.Failed++
		}
	}
	return p, true
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

func nameOf(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "not posted"
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no output"
	}
	return s
}

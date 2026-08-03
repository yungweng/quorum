// Package review runs several independent Codex reviewers against one pull
// request, merges their findings into a single comment, and posts it.
//
// This is the core the other two halves of quorum are built on: the fix
// pipeline loops over it, and the daemon triggers it. Both used to shell out to
// the pr-codex-review script and then guess what it had done, one by grepping
// its stdout for a "Run dir" line, the other by diffing directory listings of
// the run cache. Result is now simply the return value.
package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yungweng/quorum/internal/cachefs"
	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/deps"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/runname"
	"github.com/yungweng/quorum/internal/target"
)

// Failures a caller has to tell apart. The shell version signalled these as
// exit codes 2, 3 and 4, which the CLI still maps back onto.
var (
	// ErrEnvrcChanged means the target touches an .envrc and no override was given.
	ErrEnvrcChanged = errors.New("target changes an .envrc file")
	// ErrHeadDrifted means the target received a new commit while the reviewers
	// were running, so their findings describe code that is no longer current.
	ErrHeadDrifted = errors.New("target head changed during review")
	// ErrAggregatorInvalid means the aggregator could not produce a comment
	// with the required structure, twice.
	ErrAggregatorInvalid = errors.New("aggregator output invalid")
	// ErrVerifierInvalid means the evidence pass failed, produced a malformed
	// report twice, or changed the checked-out repository.
	ErrVerifierInvalid = errors.New("verifier output invalid")
	// ErrTooFewReviewers means not enough reviewer passes succeeded.
	ErrTooFewReviewers = errors.New("too few reviewer outputs")
)

// Defaults for a run.
const (
	DefaultRuns          = 6
	DefaultModel         = "gpt-5.6-terra"
	DefaultEffort        = "medium"
	DefaultReviewTimeout = 45 * time.Minute
	// prewarmTimeout bounds the single environment entry that installs
	// dependencies before the reviewers start.
	prewarmTimeout = 20 * time.Minute
	// MaxRuns is a guard against a typo costing a fortune in Codex passes.
	MaxRuns = 12
	// runRetention and depsRetention are how long run directories and shared
	// dependency trees survive without being used.
	runRetention  = 7 * 24 * time.Hour
	depsRetention = 14 * 24 * time.Hour
)

// Options configure one review run.
type Options struct {
	Repo     string // owner/repo
	Number   int
	Branch   string // internal branch target; empty resolves the current checkout
	HeadSHA  string // internal branch pin; empty reviews the resolved target head
	RepoRoot string // the checkout the worktree is created from

	Runs          int
	Concurrency   int // 0 means all at once
	Model         string
	Effort        string
	BaseBranch    string // empty uses the PR's own base
	MinSuccessful int    // 0 means a majority of Runs
	ReviewTimeout time.Duration

	Post             bool
	KeepWorktree     bool
	UseDirenv        bool
	AllowEnvrcChange bool
	ResumeRun        string // reuse a run directory and only aggregate/post

	// Paths
	RunsDir        string   // where run directories are created
	SharedRunsDirs []string // other run roots sharing the dependency cache
	DepsDir        string   // shared dependency cache root
	CodexBin       string
	DirenvBin      string
}

// Result is what a finished run produced.
type Result struct {
	RunDir                  string
	OutputDir               string
	CommentFile             string
	VerificationChangesFile string
	CommentURL              string
	Posted                  bool
	Findings                Findings
	Duration                time.Duration
	// BaseDriftNote is set when the base branch moved during the review. The
	// head staying put is what makes the findings still valid, so this is a
	// note on the comment rather than a failure.
	BaseDriftNote string
}

// Runner performs review runs.
type Runner struct {
	GH  *gh.Client
	Git git.G
	Rep Reporter
}

// Run reviews one pull request from start to finish.
func (r *Runner) Run(ctx context.Context, o Options) (*Result, error) {
	started := time.Now()
	o = o.withDefaults()
	if err := o.validate(); err != nil {
		return nil, err
	}
	rep := r.reporter()

	tgt, run, err := r.resolveRunTarget(ctx, &o)
	if err != nil {
		return nil, err
	}
	pr := tgt.PR
	baseBranch := o.BaseBranch
	if baseBranch == "" {
		baseBranch = pr.BaseRefName
	} else if !tgt.BranchOnly && baseBranch != pr.BaseRefName {
		rep.Warn(fmt.Sprintf("PR base is %q, but reviewing against %q", pr.BaseRefName, baseBranch))
	}
	baseRef := "origin/" + baseBranch
	if tgt.BranchOnly {
		rep.Info(fmt.Sprintf(
			"branch %s has no open PR; reviewing origin/%s against %s without posting",
			pr.HeadRefName, pr.HeadRefName, baseRef,
		))
		o.Post = false
	}

	// Cache collection takes this same lock from its liveness check through
	// dependency eviction. Create and claim the run while holding it so
	// collection must happen wholly before this startup or see this run as live.
	unlockCache, err := proc.LockDir(filepath.Dir(o.DepsDir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(run.output, 0o755); err != nil {
		unlockCache()
		return nil, err
	}
	releaseClaim, err := proc.Claim(run.root)
	if err != nil {
		unlockCache()
		return nil, err
	}
	defer releaseClaim()
	if o.ResumeRun == "" {
		if err := writeRunTarget(run.target, newRunTarget(o, tgt, baseBranch)); err != nil {
			return nil, fmt.Errorf("write run target metadata: %w", err)
		}
	}

	// A fresh run has not linked a dependency tree yet, so the age sweep may
	// evict trees if no older run is using them. A resumed worktree may retain
	// links left by a hard interruption; preserve their targets until recovery
	// has finished.
	removed := r.gc(ctx, o, run.root)
	unlockCache()
	if removed > 0 {
		rep.Info(fmt.Sprintf("gc: removed %d old cache dir(s)", removed))
	}

	rep.Header(RunHeader{
		Repo: o.Repo, Number: pr.Number, Title: pr.Title, Author: pr.Author.Login,
		Branch: pr.HeadRefName, BranchOnly: tgt.BranchOnly,
		BaseRef: baseRef, BaseSHA: pr.BaseRefOid, HeadSHA: pr.HeadRefOid,
		Draft: pr.IsDraft, Runs: o.Runs, Concurrency: o.Concurrency,
		Timeout: o.ReviewTimeout, Model: o.Model, Effort: o.Effort, RunDir: run.root,
	})

	// checkout prepares the worktree and tells us what was actually reviewed.
	reviewedBase, reviewedHead, err := r.checkout(ctx, o, run, tgt, baseBranch, baseRef)
	if err != nil {
		return nil, err
	}

	env := envexec.Env{Worktree: run.worktree, Direnv: o.UseDirenv, DirenvBin: o.DirenvBin}
	if o.UseDirenv {
		if err := r.allowDirenv(ctx, o, run, baseRef, env, rep); err != nil {
			return nil, err
		}
	}

	opts := codex.Options{
		Bin: o.CodexBin, Model: o.Model, Effort: o.Effort, DisableSerena: true,
	}

	var linked []string
	// Failed runs keep their worktree so --resume-run can pick them up, but the
	// symlinks into the shared cache come out either way.
	defer func() { deps.Unlink(linked) }()

	if o.ResumeRun == "" {
		linked = r.prepareDeps(ctx, o, run, env, rep)
		if err := r.runReviewers(ctx, o, run, env, opts, baseRef, rep); err != nil {
			return nil, err
		}
	}

	indices, err := reviewerOutputs(run.output)
	if err != nil {
		return nil, err
	}
	requested := o.Runs
	if o.ResumeRun != "" && len(indices) > 0 {
		requested = len(indices)
	}
	minOK := o.MinSuccessful
	if minOK == 0 {
		minOK = (requested + 1) / 2
	}
	if len(indices) < minOK {
		return nil, fmt.Errorf("%w: %d available, need %d (logs in %s)",
			ErrTooFewReviewers, len(indices), minOK, run.output)
	}
	if len(indices) < requested {
		rep.Warn(fmt.Sprintf("one or more reviewers failed; continuing with %d reviewer output(s)", len(indices)))
	}

	driftNote, err := r.checkDrift(ctx, o, tgt, baseBranch, reviewedBase, reviewedHead)
	if err != nil {
		return nil, err
	}
	if driftNote != "" {
		rep.Warn(driftNote)
	}

	if err := collectReviewers(run.output, indices); err != nil {
		return nil, err
	}

	prompt := aggregatorPrompt(promptMeta{
		URL: pr.URL, Title: pr.Title, Author: pr.Author.Login, BaseRef: baseRef,
		BaseSHA: reviewedBase, HeadSHA: reviewedHead, BaseDriftNote: driftNote,
		Branch: pr.HeadRefName, BranchOnly: tgt.BranchOnly,
	})
	if err := r.aggregate(ctx, o, run, env, opts, prompt, len(indices), rep); err != nil {
		return nil, err
	}
	if err := r.verify(ctx, o, run, env, opts, verifierPrompt(promptMeta{
		URL: pr.URL, Title: pr.Title, Author: pr.Author.Login, BaseRef: baseRef,
		BaseSHA: reviewedBase, HeadSHA: reviewedHead,
		Branch: pr.HeadRefName, BranchOnly: tgt.BranchOnly,
	}), reviewedHead, rep); err != nil {
		return nil, err
	}

	res := &Result{
		RunDir: run.root, OutputDir: run.output, CommentFile: run.comment,
		VerificationChangesFile: run.changes,
		BaseDriftNote:           driftNote,
	}
	res.Findings = Findings{
		Schema: 1, PR: pr.Number, HeadSHA: reviewedHead, BaseSHA: reviewedBase,
		ReviewersSucceeded: len(indices), ReviewersRequested: requested,
		Blockers:    CountFile(run.comment, "Blockers"),
		Critical:    CountFile(run.comment, "Critical"),
		Suggestions: CountFile(run.comment, "Suggestions"),
		Questions:   CountFile(run.comment, "Questions"),
		CommentFile: run.comment,
	}

	rep.Comment(run.comment)

	if o.Post {
		url, err := r.postVerifiedComment(ctx, o, tgt, baseBranch, reviewedBase, reviewedHead, run.comment)
		if err != nil {
			return nil, fmt.Errorf("posting the comment: %w", err)
		}
		res.CommentURL, res.Posted = url, true
		res.Findings.Posted = true
		if url != "" {
			res.Findings.CommentURL = &url
		}
	}

	if err := writeFindings(run.findings, res.Findings); err != nil {
		return nil, err
	}
	res.Duration = time.Since(started)

	// The worktree is the bulk of a run directory; the output stays readable
	// without it. A failed run never reaches here, which is what keeps
	// --resume-run working.
	if !o.KeepWorktree {
		deps.Unlink(linked)
		linked = nil
		r.Git.WorktreeRemove(ctx, o.RepoRoot, run.worktree)
	}
	rep.Done(*res)
	return res, nil
}

// postVerifiedComment rechecks the remote head after verification, which can
// take as long as another review pass. Findings for an older head must never be
// posted on the current pull request.
func (r *Runner) postVerifiedComment(ctx context.Context, o Options, tgt target.Target, baseBranch, reviewedBase, reviewedHead, comment string) (string, error) {
	if _, err := r.checkDrift(ctx, o, tgt, baseBranch, reviewedBase, reviewedHead); err != nil {
		return "", err
	}
	return r.GH.Comment(ctx, o.RepoRoot, tgt.PR.Number, comment)
}

// runPaths are the files one run works with.
type runPaths struct {
	root      string
	output    string
	worktree  string
	target    string
	comment   string
	candidate string
	changes   string
	all       string
	findings  string
}

func (o Options) runDir(number int, branch string) (runPaths, error) {
	root := o.ResumeRun
	if root == "" {
		stamp := time.Now().Format("20060102-150405")
		name := fmt.Sprintf("pr-%d", number)
		if number == 0 {
			name = "branch-" + runname.BranchPart(branch)
		}
		root = filepath.Join(o.RunsDir, fmt.Sprintf("%s-%s-%s",
			strings.ReplaceAll(o.Repo, "/", "-"), name, stamp))
	} else {
		root = strings.TrimSuffix(root, "/")
		abs, err := filepath.Abs(root)
		if err != nil {
			return runPaths{}, err
		}
		root = abs
	}
	out := filepath.Join(root, "output")
	return runPaths{
		root:      root,
		output:    out,
		worktree:  filepath.Join(root, "worktree"),
		target:    filepath.Join(root, "target.json"),
		comment:   filepath.Join(out, "final-pr-comment.md"),
		candidate: filepath.Join(out, "aggregated-pr-comment.md"),
		changes:   filepath.Join(out, "verification-changes.md"),
		all:       filepath.Join(out, "all-reviewers.md"),
		findings:  filepath.Join(out, "findings.json"),
	}, nil
}

// checkout puts the target head into a detached worktree and returns the base
// and head SHAs that the reviewers will actually see.
func (r *Runner) checkout(ctx context.Context, o Options, run runPaths, tgt target.Target, baseBranch, baseRef string) (baseSHA, headSHA string, err error) {
	pr := tgt.PR
	if o.ResumeRun != "" {
		if _, err := os.Stat(run.output); err != nil {
			return "", "", fmt.Errorf("resume output directory does not exist: %s", run.output)
		}
		if _, err := os.Stat(filepath.Join(run.worktree, ".git")); err != nil {
			return "", "", fmt.Errorf("resume worktree does not look like a git checkout: %s", run.worktree)
		}
		base, err := r.Git.RevParse(ctx, run.worktree, baseRef)
		if err != nil {
			return "", "", err
		}
		head, err := r.Git.RevParse(ctx, run.worktree, "HEAD")
		if err != nil {
			return "", "", err
		}
		return base, head, nil
	}

	if err := r.Git.Fetch(ctx, o.RepoRoot, "origin",
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", baseBranch, baseBranch)); err != nil {
		return "", "", err
	}
	base, err := r.Git.RevParse(ctx, o.RepoRoot, baseRef)
	if err != nil {
		return "", "", err
	}

	headRef := fmt.Sprintf("refs/quorum/pr/%d", pr.Number)
	fetchRef := fmt.Sprintf("+pull/%d/head:%s", pr.Number, headRef)
	if tgt.BranchOnly {
		headRef = "refs/remotes/origin/" + pr.HeadRefName
		fetchRef = fmt.Sprintf("+refs/heads/%s:%s", pr.HeadRefName, headRef)
	}
	if err := r.Git.Fetch(ctx, o.RepoRoot, "origin", fetchRef); err != nil {
		return "", "", err
	}
	head, err := r.Git.RevParse(ctx, o.RepoRoot, headRef)
	if err != nil {
		return "", "", err
	}
	// GitHub's metadata and the ref must agree. When they do not, something
	// moved between the two calls and the run would review one state while
	// reporting another.
	if head != pr.HeadRefOid {
		return "", "", fmt.Errorf("fetched target head %s differs from resolved metadata %s", head, pr.HeadRefOid)
	}
	if err := r.Git.WorktreeAdd(ctx, o.RepoRoot, run.worktree, head); err != nil {
		return "", "", err
	}
	return base, head, nil
}

// allowDirenv runs `direnv allow` in the worktree, unless the PR itself changed
// an .envrc. That refusal is the point: allowing means executing whatever the
// PR author put in that file, on this machine, before any human read it.
func (r *Runner) allowDirenv(ctx context.Context, o Options, run runPaths, baseRef string, env envexec.Env, rep Reporter) error {
	if !hasEnvrc(run.worktree) {
		return nil
	}
	changed, err := r.Git.ChangedFiles(ctx, run.worktree, baseRef+"...HEAD", ".envrc", ":(glob)**/.envrc")
	if err != nil {
		return fmt.Errorf("check changed .envrc files: %w", err)
	}
	if changed != "" && !o.AllowEnvrcChange {
		return fmt.Errorf("%w:\n%s\nReview the .envrc change manually, then rerun with --allow-envrc-change if it is safe",
			ErrEnvrcChanged, changed)
	}
	if err := env.Allow(ctx); err != nil {
		return fmt.Errorf("direnv allow: %w", err)
	}
	rep.Info("direnv:   allowed")
	return nil
}

// prepareDeps links in cached dependency trees, enters the environment once so
// the project's install hook runs at most one time, and publishes whatever it
// installed for the next run.
func (r *Runner) prepareDeps(ctx context.Context, o Options, run runPaths, env envexec.Env, rep Reporter) []string {
	cache := deps.Cache{
		Root:     o.DepsDir,
		Repo:     strings.ReplaceAll(o.Repo, "/", "-"),
		Worktree: run.worktree,
	}
	links, reused, err := cache.Link()
	if err != nil {
		rep.Warn(fmt.Sprintf("dependency cache lookup failed: %v", err))
	}
	for _, p := range reused {
		rep.Info("deps:     reused " + p.String())
	}

	if o.UseDirenv {
		started := time.Now()
		logFile, _ := os.Create(filepath.Join(run.output, "prewarm.log"))
		err := env.Run(ctx, prewarmTimeout, envexec.Cmd{
			Name: "true", Stdout: logFile, Stderr: logFile,
		})
		if logFile != nil {
			logFile.Close()
		}
		if err != nil {
			// Not fatal: the reviewers set the environment up themselves,
			// exactly as they did before this step existed. It just costs one
			// install per reviewer instead of one per run.
			rep.Warn(fmt.Sprintf("environment prewarm failed, see %s/prewarm.log", run.output))
		} else {
			rep.Info(fmt.Sprintf("prewarm:  environment ready in %s", fmtDuration(time.Since(started))))
		}
	}

	captured, cached, err := cache.Capture()
	if err != nil {
		rep.Warn(fmt.Sprintf("publishing dependency trees failed: %v", err))
	}
	for _, p := range cached {
		rep.Info("deps:     cached " + p.String())
	}
	return append(links, captured...)
}

// runReviewers runs the reviewer passes, at most Concurrency at a time.
func (r *Runner) runReviewers(ctx context.Context, o Options, run runPaths, env envexec.Env, opts codex.Options, baseRef string, rep Reporter) error {
	started := time.Now()
	// Opens the event log before the first reviewer finishes, so a watcher can
	// tell "the reviewers are running and none has finished" apart from "the
	// worktree and its dependencies are still being prepared". Without it every
	// run looks like it is starting up until the first reviewer returns, which
	// on a cold dependency cache is several minutes in.
	appendEvent(run.output, "start")
	sem := make(chan struct{}, o.Concurrency)
	var wg sync.WaitGroup

	var mu sync.Mutex
	done, failed := 0, 0

	// A ticker drives the progress line so it keeps moving while reviewers are
	// quiet; reviewers themselves only report when they finish.
	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				mu.Lock()
				d, f := done, failed
				mu.Unlock()
				running := min(o.Runs-d-f, o.Concurrency)
				rep.Progress(d, f, running, time.Since(started))
			}
		}
	}()

	for idx := 1; idx <= o.Runs; idx++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			out := filepath.Join(run.output, fmt.Sprintf("reviewer-%d.md", idx))
			logPath := filepath.Join(run.output, fmt.Sprintf("reviewer-%d.log", idx))
			logFile, err := os.Create(logPath)
			if err != nil {
				return
			}
			defer logFile.Close()

			rep.ReviewerStarted(idx)
			begin := time.Now()
			err = opts.Review(ctx, env, o.ReviewTimeout, baseRef, out, logFile)
			elapsed := time.Since(begin)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// A partial output file would be aggregated as if it were a
				// complete review, so it goes.
				os.Remove(out)
				failed++
				appendEvent(run.output, fmt.Sprintf("fail %d %d %d",
					idx, proc.ExitCode(err), int(elapsed.Seconds())))
				rep.ReviewerFailed(idx, proc.ExitCode(err), elapsed, logPath)
				return
			}
			done++
			appendEvent(run.output, fmt.Sprintf("ok %d %d %d", idx, int(elapsed.Seconds()), done))
			rep.ReviewerDone(idx, elapsed, done, o.Runs)
		}()
	}

	wg.Wait()
	close(progressDone)
	return ctx.Err()
}

// checkDrift compares what was reviewed against what the remote holds now.
func (r *Runner) checkDrift(ctx context.Context, o Options, tgt target.Target, baseBranch, reviewedBase, reviewedHead string) (string, error) {
	latestBase, err := r.Git.LsRemote(ctx, o.RepoRoot, "origin", "refs/heads/"+baseBranch)
	if err != nil {
		return "", err
	}
	headRef := fmt.Sprintf("refs/pull/%d/head", tgt.PR.Number)
	if tgt.BranchOnly {
		headRef = "refs/heads/" + tgt.PR.HeadRefName
	}
	latestHead, err := r.Git.LsRemote(ctx, o.RepoRoot, "origin", headRef)
	if err != nil {
		return "", err
	}
	// A moved head means the reviewers described code the target no longer
	// contains. Publishing that would be worse than publishing nothing.
	if latestHead != reviewedHead {
		return "", fmt.Errorf("%w: reviewed %s, latest %s (base reviewed %s, latest %s)",
			ErrHeadDrifted, reviewedHead, latestHead, reviewedBase, latestBase)
	}
	// A moved base is survivable: the target head stayed fixed. It goes into
	// the report as a note.
	if latestBase != reviewedBase {
		return fmt.Sprintf("Base branch moved after review: reviewed %s, latest %s. The target head stayed the same.",
			reviewedBase, latestBase), nil
	}
	return "", nil
}

// aggregate merges the reviewer outputs into the candidate comment, retrying
// once when the structure is wrong. A run that still cannot produce it fails
// rather than passing malformed input to the verifier.
func (r *Runner) aggregate(ctx context.Context, o Options, run runPaths, env envexec.Env, opts codex.Options, prompt string, reviewers int, rep Reporter) error {
	logPath := filepath.Join(run.output, "aggregator.log")
	var lastErr error
	appendEvent(run.output, "aggregate")

	for attempt := 1; attempt <= 2; attempt++ {
		rep.Aggregating(reviewers, attempt)

		logFile, err := os.Create(logPath)
		if err != nil {
			return err
		}
		all, err := os.Open(run.all)
		if err != nil {
			logFile.Close()
			return err
		}
		// The shell version ran the aggregator without any timeout at all, so a
		// hung pass stalled the run indefinitely. It gets the reviewer timeout.
		err = opts.Aggregate(ctx, env, o.ReviewTimeout, prompt, run.candidate, all, logFile)
		all.Close()
		logFile.Close()

		if err != nil {
			lastErr = fmt.Errorf("codex exited nonzero, see %s", logPath)
		} else if lastErr = ValidateComment(run.candidate); lastErr == nil {
			return nil
		}
		if attempt == 1 {
			rep.Warn(fmt.Sprintf("aggregator: attempt 1 produced invalid output (%v), retrying", lastErr))
		}
	}
	return fmt.Errorf("%w after 2 attempts (%v); refusing to post, see %s",
		ErrAggregatorInvalid, lastErr, run.candidate)
}

// verify independently checks every aggregated finding against the worktree.
// The model may refine the report, but HEAD and the complete porcelain status
// must remain unchanged. Any repository mutation fails immediately; malformed
// report output gets one retry. Nothing is posted before both gates pass.
func (r *Runner) verify(ctx context.Context, o Options, run runPaths, env envexec.Env, opts codex.Options, prompt, expectedHead string, rep Reporter) error {
	logPath := filepath.Join(run.output, "verifier.log")
	var lastErr error
	appendEvent(run.output, "verify")
	if err := r.verifierWorktreeUnchanged(ctx, run.worktree, expectedHead); err != nil {
		return fmt.Errorf("%w before execution: %v", ErrVerifierInvalid, err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		rep.Verifying(attempt)
		attemptPrompt := prompt
		if attempt > 1 {
			attemptPrompt += fmt.Sprintf("\n\nYour previous output was rejected: %v. Correct that error and return the complete report again.", lastErr)
		}

		logFile, err := os.Create(logPath)
		if err != nil {
			return err
		}
		candidate, err := os.Open(run.candidate)
		if err != nil {
			logFile.Close()
			return err
		}
		err = opts.Verify(ctx, env, o.ReviewTimeout, attemptPrompt, run.comment, candidate, logFile)
		candidate.Close()
		logFile.Close()
		if stateErr := r.verifierWorktreeUnchanged(ctx, run.worktree, expectedHead); stateErr != nil {
			return fmt.Errorf("%w: %v; refusing to retry or post", ErrVerifierInvalid, stateErr)
		}

		validationErr := ValidateComment(run.comment)
		switch {
		case err != nil:
			lastErr = fmt.Errorf("codex exited nonzero, see %s", logPath)
		case validationErr != nil:
			lastErr = validationErr
		default:
			if err := writeVerificationChanges(run.candidate, run.comment, run.changes); err != nil {
				return err
			}
			return nil
		}
		if attempt == 1 {
			rep.Warn(fmt.Sprintf("verifier: attempt 1 produced invalid output (%v), retrying", lastErr))
		}
	}
	return fmt.Errorf("%w after 2 attempts (%v); refusing to post, see %s",
		ErrVerifierInvalid, lastErr, run.comment)
}

func (r *Runner) verifierWorktreeUnchanged(ctx context.Context, worktree, expectedHead string) error {
	head, err := r.Git.RevParse(ctx, worktree, "HEAD")
	if err != nil {
		return fmt.Errorf("checking verifier HEAD: %w", err)
	}
	if head != expectedHead {
		return fmt.Errorf("verifier changed HEAD from %s to %s", expectedHead, head)
	}
	status, err := r.Git.StatusPorcelain(ctx, worktree)
	if err != nil {
		return fmt.Errorf("checking verifier worktree status: %w", err)
	}
	if status != "" {
		return fmt.Errorf("verifier left staged, unstaged, or untracked changes:\n%s", status)
	}
	return nil
}

// gc drops run directories and shared dependency trees nothing has needed for a
// while, and clears worktree registrations left behind by killed runs.
func (r *Runner) gc(ctx context.Context, o Options, current string) int {
	removed := 0
	otherLive := false
	entries, err := os.ReadDir(o.RunsDir)
	if err == nil {
		cutoff := time.Now().Add(-runRetention)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(o.RunsDir, e.Name())
			if proc.Claimed(dir) {
				if dir != current {
					otherLive = true
				}
				continue
			}
			info, err := e.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			// Unregister before deleting, or git keeps a dangling worktree
			// entry that later prunes have to clean up.
			r.Git.WorktreeRemove(ctx, o.RepoRoot, filepath.Join(dir, "worktree"))
			if cachefs.RemoveAll(dir) == nil {
				removed++
			}
		}
	} else if !os.IsNotExist(err) {
		// An unreadable run root may contain a live claim. Preserve shared
		// trees rather than severing a dependency link we could not inspect.
		otherLive = true
	}
	for _, root := range o.SharedRunsDirs {
		if claimedRunIn(root) {
			otherLive = true
			break
		}
	}
	// A live run may have a node_modules symlink into any shared tree. A
	// fresh current run is safe to exclude because startup holds the cache lock
	// and it has not linked anything yet; ResumeRun covers retained worktrees.
	if !otherLive && o.ResumeRun == "" {
		cache := deps.Cache{Root: o.DepsDir}
		removed += cache.GC(depsRetention)
	}
	r.Git.WorktreePrune(ctx, o.RepoRoot)
	return removed
}

// claimedRunIn reports conservatively: an unreadable root may hide a live
// claim, while a root that has never been created cannot.
func claimedRunIn(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, e := range entries {
		if e.IsDir() && proc.Claimed(filepath.Join(root, e.Name())) {
			return true
		}
	}
	return false
}

func (o Options) withDefaults() Options {
	if o.Runs == 0 {
		o.Runs = DefaultRuns
	}
	if o.Concurrency == 0 {
		o.Concurrency = o.Runs
	}
	if o.Model == "" {
		o.Model = DefaultModel
	}
	if o.Effort == "" {
		o.Effort = DefaultEffort
	}
	if o.ReviewTimeout == 0 {
		o.ReviewTimeout = DefaultReviewTimeout
	}
	return o
}

func (o Options) validate() error {
	if o.Runs < 1 {
		return fmt.Errorf("runs must be >= 1")
	}
	if o.Runs > MaxRuns {
		return fmt.Errorf("runs must be <= %d", MaxRuns)
	}
	if o.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1")
	}
	if o.MinSuccessful < 0 {
		return fmt.Errorf("min-successful must be >= 1")
	}
	if !codex.ValidEffort(o.Effort) {
		return fmt.Errorf("effort must be one of: %s", strings.Join(codex.Efforts, ", "))
	}
	if o.Repo == "" || o.RepoRoot == "" {
		return fmt.Errorf("repo and repo root are required")
	}
	return nil
}

func (r *Runner) reporter() Reporter {
	if r.Rep == nil {
		return NopReporter{}
	}
	return r.Rep
}

// reviewerOutputs lists the indices of reviewer passes that produced output.
func reviewerOutputs(dir string) ([]int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "reviewer-*.md"))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, m := range matches {
		var idx int
		if _, err := fmt.Sscanf(filepath.Base(m), "reviewer-%d.md", &idx); err == nil {
			out = append(out, idx)
		}
	}
	sort.Ints(out)
	return out, nil
}

// collectReviewers concatenates the reviewer outputs into the aggregator input.
func collectReviewers(dir string, indices []int) error {
	var b strings.Builder
	b.WriteString("# Reviewer Outputs\n\n")
	for _, idx := range indices {
		path := filepath.Join(dir, fmt.Sprintf("reviewer-%d.md", idx))
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("missing reviewer output: %s", path)
		}
		fmt.Fprintf(&b, "## Reviewer %d\n\n%s\n", idx, data)
	}
	return os.WriteFile(filepath.Join(dir, "all-reviewers.md"), []byte(b.String()), 0o644)
}

func hasEnvrc(root string) bool {
	found := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is not an answer
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == ".envrc" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func fmtDuration(d time.Duration) string {
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// appendEvent records one reviewer outcome in the run's event log.
//
// The log is how a separate process watches a review in flight: the daemon's
// dashboard counts these lines to show "4/6 reviewers done" for a run it did
// not start and cannot call back into. Its existence is itself the signal that
// the reviewers have started, which is why runReviewers writes a "start" line
// before any of them can finish. Outcome lines are appended under the reviewer
// mutex and the start line is written before the reviewers exist, so the
// appends are already serialised.
func appendEvent(outputDir, line string) {
	f, err := os.OpenFile(filepath.Join(outputDir, "events.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

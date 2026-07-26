package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yungweng/prbot/internal/config"
	"github.com/yungweng/prbot/internal/logbook"
	"github.com/yungweng/prbot/internal/paths"
	"github.com/yungweng/prbot/internal/state"
)

// Runner performs one review from start to finish.
type Runner struct {
	Cfg       config.Config
	P         paths.P
	Log       *logbook.Logger
	ReviewBin string
	GitBin    string
	GHBin     string
}

// Findings is the machine readable summary pr-codex-review writes.
type Findings struct {
	Blockers           int    `json:"blockers"`
	Critical           int    `json:"critical"`
	Suggestions        int    `json:"suggestions"`
	Questions          int    `json:"questions"`
	ReviewersSucceeded int    `json:"reviewers_succeeded"`
	ReviewersRequested int    `json:"reviewers_requested"`
	Posted             bool   `json:"posted"`
	CommentURL         string `json:"comment_url"`
	HeadSHA            string `json:"head_sha"`
}

// Review clones or refreshes the repository, runs pr-codex-review against the
// pull request and records the outcome. It is called in the detached child
// process that owns the marker.
func (r *Runner) Review(ctx context.Context, key, repo string, number int, sha, title, reqAt string) error {
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
		rec.Mark(state.Running, "")
		rec.SHA = sha
		rec.RunLog = runLog
		rec.RunDir = ""
		rec.StartedAt = state.Now()
	})

	runDir, runErr := r.exec(ctx, clone, repo, number, runLog, func(dir string) {
		// pr-codex-review prints its run directory early. Recording it lets
		// status follow the reviewers while the review is still going.
		r.mutate(key, func(rec *state.Record) { rec.RunDir = dir })
	})

	findings, ferr := readFindings(runDir)
	if runErr == nil && ferr == nil {
		r.Log.Printf("%s: done, %d blockers %d critical %d suggestions %d questions -> %s",
			key, findings.Blockers, findings.Critical, findings.Suggestions, findings.Questions,
			orDash(findings.CommentURL))
		r.mutate(key, func(rec *state.Record) {
			rec.Mark(state.OK, "")
			rec.SHA = sha
			rec.RunDir = runDir
			rec.CommentURL = findings.CommentURL
			rec.Blockers = state.Num(findings.Blockers)
			rec.Critical = state.Num(findings.Critical)
			rec.Suggestions = state.Num(findings.Suggestions)
			rec.Questions = state.Num(findings.Questions)
			rec.Fails = 0
			// Recording the request timestamp is what marks it as handled.
			rec.ReqAt = reqAt
		})
		r.notify(fmt.Sprintf("Reviewed %s#%d", nameOf(repo), number),
			summaryLine(findings), findings.CommentURL)
		return nil
	}

	reason := "pr-codex-review failed"
	if runErr != nil {
		reason = runErr.Error()
	} else if ferr != nil {
		reason = fmt.Sprintf("no findings written: %v", ferr)
	}
	r.Log.Printf("%s: FAILED, %s (log: %s)", key, reason, runLog)
	// The request timestamp stays unrecorded on purpose, so the next poll
	// retries until MaxRetries is reached.
	r.recordFailureWith(key, sha, runDir, reason, runLog)
	r.notify(fmt.Sprintf("Review failed: %s#%d", nameOf(repo), number), reason, "")
	return fmt.Errorf("%s: %s", key, reason)
}

// mutate updates the state file and complains loudly if it cannot. Losing this
// write means losing the outcome of a review that has already been paid for.
func (r *Runner) mutate(key string, fn func(*state.Record)) {
	if err := state.Mutate(r.P.StateFile, key, fn); err != nil {
		r.Log.Printf("%s: could not record the result: %v", key, err)
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

// exec runs pr-codex-review and returns the run directory it reported.
func (r *Runner) exec(ctx context.Context, dir, repo string, number int, runLog string, onRunDir func(string)) (string, error) {
	args := []string{fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), "--no-notify"}
	// No --concurrency: pr-codex-review then runs every pass at once, which is
	// the point. Anyone who wants it throttled can say so in REVIEW_ARGS.
	if r.Cfg.Reviewers > 0 {
		args = append(args, "-n", fmt.Sprint(r.Cfg.Reviewers))
	}
	args = append(args, strings.Fields(r.Cfg.ReviewArgs)...)

	f, err := os.Create(runLog)
	if err != nil {
		return "", err
	}
	defer f.Close()

	tee := &runDirScanner{out: f, found: onRunDir}
	cmd := exec.CommandContext(ctx, r.ReviewBin, args...)
	cmd.Dir = dir
	cmd.Stdout = tee
	cmd.Stderr = tee
	cmd.Env = os.Environ()
	err = cmd.Run()
	tee.flush()
	if err != nil {
		return tee.dir, fmt.Errorf("pr-codex-review: %w", err)
	}
	return tee.dir, nil
}

// runDirScanner passes everything through to the log while watching for the
// "Run dir" line of pr-codex-review's header.
type runDirScanner struct {
	out   io.Writer
	found func(string)
	buf   bytes.Buffer
	dir   string
}

func (s *runDirScanner) Write(p []byte) (int, error) {
	n, err := s.out.Write(p)
	if s.dir == "" {
		s.buf.Write(p[:n])
		s.scan()
	}
	return n, err
}

func (s *runDirScanner) scan() {
	for s.dir == "" {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// Incomplete line: put it back and wait for the rest.
			s.buf.Reset()
			s.buf.WriteString(line)
			return
		}
		if dir, ok := parseRunDir(line); ok {
			s.dir = dir
			if s.found != nil {
				s.found(dir)
			}
		}
	}
}

func (s *runDirScanner) flush() {
	if s.dir == "" {
		if dir, ok := parseRunDir(s.buf.String()); ok {
			s.dir = dir
		}
	}
}

// parseRunDir reads the "  Run dir    /path" line of the header.
func parseRunDir(line string) (string, bool) {
	t := strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(t, "Run dir")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || !strings.HasPrefix(rest, "/") {
		return "", false
	}
	return rest, true
}

func readFindings(runDir string) (Findings, error) {
	var f Findings
	if runDir == "" {
		return f, fmt.Errorf("pr-codex-review did not report a run directory")
	}
	b, err := os.ReadFile(filepath.Join(runDir, "output", "findings.json"))
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, err
	}
	return f, nil
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

// Progress is what a running review reports about itself, read from
// pr-codex-review's event log.
type Progress struct {
	Done      int
	Failed    int
	Requested int
}

// ReadProgress counts finished reviewers of a run in flight. A run directory
// that is not there yet simply reports nothing.
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

func summaryLine(f Findings) string {
	if f.Blockers == 0 && f.Critical == 0 && f.Suggestions == 0 && f.Questions == 0 {
		return "nothing found"
	}
	return fmt.Sprintf("%d blockers, %d critical, %d suggestions", f.Blockers, f.Critical, f.Suggestions)
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

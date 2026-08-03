package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/codex"
)

const (
	finalDescriptionTimeout = 30 * time.Minute
	maxPRDescriptionBytes   = 64 * 1024
)

// finishPRDescription writes a final PR body candidate after the pipeline has
// converged. It uses a fresh read-only session rather than the fix session so
// the body describes the final state instead of narrating the fix history.
// GitHub has no conditional body-update API, so the candidate stays local: an
// unconditional update could overwrite a human edit or target a moved head.
func (r *run) finishPRDescription(res *Result) error {
	if r.target.BranchOnly || !r.o.Post {
		return nil
	}

	r.enter(PhaseDescription)
	r.rep.Step("PR description")
	label := "Final PR description"
	r.rep.StepStart(label)
	started := time.Now()
	ok := false
	defer func() { r.rep.StepEnd(label, time.Since(started), ok) }()

	finalHead, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return err
	}
	if err := r.checkPRDescriptionTarget(finalHead); err != nil {
		return err
	}

	bodyPath := filepath.Join(r.msgDir, "final-pr-description.md")
	logPath := filepath.Join(r.logDir, "final-pr-description.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	safe := codex.Options{
		Bin: r.o.CodexBin, Model: r.o.ReviewModel, Effort: r.o.ReviewEffort,
		DisableSerena: true,
	}
	err = safe.DescribePR(r.ctx, r.env, finalDescriptionTimeout,
		finalDescriptionPrompt(r.pr.Number, r.pr.Title, r.pr.BaseRefName),
		bodyPath, strings.NewReader(r.pr.Body), logFile)
	logFile.Close()
	if err != nil {
		return fmt.Errorf("generating final PR description: %w; see %s", err, logPath)
	}
	if err := r.ensureDescriptionWorktreeUnchanged(finalHead); err != nil {
		return err
	}

	body, err := readFinalPRDescription(bodyPath)
	if err != nil {
		return err
	}
	res.PRDescriptionFile = bodyPath
	if err := r.checkPRDescriptionTarget(finalHead); err != nil {
		return err
	}
	if strings.TrimSpace(body) == strings.TrimSpace(r.pr.Body) {
		res.PRDescriptionCurrent = true
		ok = true
		return nil
	}
	r.rep.Warn(fmt.Sprintf("GitHub has no conditional PR-description update; generated candidate left at %s", bodyPath))
	ok = true
	return nil
}

// checkPRDescriptionTarget rejects a candidate based on stale PR state.
func (r *run) checkPRDescriptionTarget(expectedHead string) error {
	latest, err := r.p.GH.ViewPR(r.ctx, r.o.RepoRoot, r.pr.Number)
	if err != nil {
		return fmt.Errorf("checking PR before generating its final description: %w", err)
	}
	if latest.State != "OPEN" {
		return fmt.Errorf("PR #%d became %s before its final description could be generated", r.pr.Number, latest.State)
	}
	if latest.HeadRefOid != expectedHead {
		return fmt.Errorf("PR head moved during final description generation: expected %s, got %s", expectedHead, latest.HeadRefOid)
	}
	if latest.Body != r.pr.Body {
		return fmt.Errorf("PR description changed during babysit; rejecting the stale generated candidate")
	}
	return nil
}

func (r *run) ensureDescriptionWorktreeUnchanged(expectedHead string) error {
	head, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return err
	}
	if head != expectedHead {
		return fmt.Errorf("PR description pass changed HEAD from %s to %s", expectedHead, head)
	}
	status, err := r.p.Git.StatusPorcelain(r.ctx, r.worktree)
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("PR description pass left repository changes: %s", dirtyStatusPreview(status))
	}
	return nil
}

func readFinalPRDescription(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading final PR description: %w", err)
	}
	if len(data) > maxPRDescriptionBytes {
		return "", fmt.Errorf("final PR description is %d bytes; maximum is %d", len(data), maxPRDescriptionBytes)
	}
	body := strings.TrimSpace(string(data))
	if body == "" {
		return "", fmt.Errorf("final PR description is empty")
	}
	if strings.HasPrefix(body, "```") {
		return "", fmt.Errorf("final PR description is wrapped in a code fence")
	}
	body += "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("normalizing final PR description: %w", err)
	}
	return body, nil
}

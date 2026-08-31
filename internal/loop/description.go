package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/engine"
)

const (
	finalDescriptionTimeout = 30 * time.Minute
	maxPRDescriptionBytes   = 64 * 1024
)

// finishPRDescription writes the final PR body after the pipeline has
// converged. It uses a fresh read-only session rather than the fix session so
// the body describes the final state instead of narrating the fix history.
// GitHub has no conditional body-update API, so the closest safe equivalent is
// used: re-read the PR and compare immediately before writing, and reject the
// candidate on any drift in state, head or description. The remaining window
// between that read and the write is one gh call wide.
func (r *run) finishPRDescription(res *Result) error {
	if r.target.BranchOnly || !r.o.Post {
		return nil
	}

	r.enter(PhaseDescription)
	r.rep.Step("PR description")
	label := "Final PR description"
	r.rep.StepStart(label, r.reviewModel)
	started := time.Now()
	ok := false
	defer func() { r.rep.StepEnd(label, r.reviewModel, time.Since(started), ok) }()

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
	m := r.reviewModel
	safe, err := engine.NewReviewer(m.Engine, engine.ReviewerOptions{
		Bin: r.o.engineBin(m.Engine), Model: m.Name, Effort: m.Effort,
	})
	if err != nil {
		logFile.Close()
		return err
	}
	err = await(r.ctx, progressInterval, func(elapsed time.Duration) {
		if elapsed > 0 {
			r.rep.StepTick(label, r.reviewModel, elapsed)
		}
	}, func() error {
		return safe.DescribePR(r.ctx, r.env, finalDescriptionTimeout,
			finalDescriptionPrompt(r.pr.Number, r.pr.Title, r.pr.BaseRefName),
			bodyPath, strings.NewReader(r.pr.Body), logFile)
	})
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
	if err := r.p.GH.EditPRBody(r.ctx, r.o.RepoRoot, r.pr.Number, bodyPath); err != nil {
		return fmt.Errorf("updating PR description: %w; candidate left at %s", err, bodyPath)
	}
	res.PRDescriptionUpdated = true
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

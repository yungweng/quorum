package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// FullPR is the pull request metadata the review core and the fix pipeline
// need. The daemon's lighter Details type stays as it is; this is the superset
// a run works from.
type FullPR struct {
	Number            int    `json:"number"`
	Title             string `json:"title"`
	Body              string `json:"body"`
	URL               string `json:"url"`
	State             string `json:"state"`
	IsDraft           bool   `json:"isDraft"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	BaseRefName       string `json:"baseRefName"`
	BaseRefOid        string `json:"baseRefOid"`
	Author            struct {
		Login string `json:"login"`
	} `json:"author"`
}

const fullPRFields = "number,title,body,url,state,isDraft,isCrossRepository," +
	"headRefName,headRefOid,baseRefName,baseRefOid,author"

// ViewPR fetches full metadata. A number of 0 means "the PR of the current
// branch", which is what `quorum babysit` without an argument uses.
func (c *Client) ViewPR(ctx context.Context, dir string, number int) (FullPR, error) {
	args := []string{"pr", "view"}
	if number > 0 {
		args = append(args, fmt.Sprint(number))
	}
	args = append(args, "--json", fullPRFields)

	out, err := c.runIn(ctx, dir, args...)
	if err != nil {
		return FullPR{}, err
	}
	var pr FullPR
	if err := json.Unmarshal(out, &pr); err != nil {
		return FullPR{}, fmt.Errorf("gh pr view: %w", err)
	}
	return pr, nil
}

// OpenPRNumbersForBranch returns the open pull requests whose head has the
// given branch name. The caller decides whether zero, one or several matches
// are valid for its workflow.
func (c *Client) OpenPRNumbersForBranch(ctx context.Context, dir, branch string) ([]int, error) {
	out, err := c.runIn(ctx, dir, "pr", "list", "--head", branch, "--state", "open",
		"--limit", "2", "--json", "number")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("gh pr list --head %s: %w", branch, err)
	}
	numbers := make([]int, 0, len(rows))
	for _, row := range rows {
		if row.Number > 0 {
			numbers = append(numbers, row.Number)
		}
	}
	return numbers, nil
}

// DefaultBranch returns the repository's default branch for the checkout.
func (c *Client) DefaultBranch(ctx context.Context, dir string) (string, error) {
	out, err := c.runIn(ctx, dir, "repo", "view", "--json", "defaultBranchRef",
		"-q", ".defaultBranchRef.name")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("%w: gh repo view returned no default branch", ErrTransient)
	}
	return branch, nil
}

// CurrentRepo returns "owner/repo" for the checkout in dir.
func (c *Client) CurrentRepo(ctx context.Context, dir string) (string, error) {
	out, err := c.runIn(ctx, dir, "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	if err != nil {
		return "", err
	}
	repo := strings.TrimSpace(string(out))
	if repo == "" {
		return "", fmt.Errorf("%w: gh repo view returned no name", ErrTransient)
	}
	return repo, nil
}

// HeadSHA asks GitHub what it currently considers the PR head. Used after a
// push to confirm the new head landed before any check result is trusted.
func (c *Client) HeadSHA(ctx context.Context, dir string, number int) (string, error) {
	out, err := c.runIn(ctx, dir, "pr", "view", fmt.Sprint(number), "--json", "headRefOid", "-q", ".headRefOid")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Comment posts a comment from a file and returns its URL.
func (c *Client) Comment(ctx context.Context, dir string, number int, bodyFile string) (string, error) {
	out, err := c.runIn(ctx, dir, "pr", "comment", fmt.Sprint(number), "--body-file", bodyFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CommentBody posts a comment from a string and returns its URL.
func (c *Client) CommentBody(ctx context.Context, dir string, number int, body string) (string, error) {
	out, err := c.runIn(ctx, dir, "pr", "comment", fmt.Sprint(number), "--body", body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CheckState is the outcome of one CI observation.
type CheckState int

const (
	// ChecksPass means every required check succeeded.
	ChecksPass CheckState = iota
	// ChecksFail means at least one check failed or was cancelled.
	ChecksFail
	// ChecksNone means GitHub reports no checks at all for this PR.
	ChecksNone
	// ChecksPending means checks exist but have not settled yet.
	ChecksPending
)

// ghChecksPendingExit is what `gh pr checks` returns while checks are still
// running. With --watch it should have settled, but a watch can return on a
// state change that is not terminal.
const ghChecksPendingExit = 8

// WatchChecks blocks until CI settles, mirroring `gh pr checks --watch
// --fail-fast`. It returns the state plus the tail of gh's output for logging.
//
// This deliberately bypasses the retry client: the call is expected to run for
// as long as CI does, so the short per-call timeout and the retry policy would
// both be wrong. Transient GitHub errors surface as ChecksFail and the caller's
// own retry loop handles them.
func (c *Client) WatchChecks(ctx context.Context, dir string, number int) (CheckState, string, error) {
	cmd := exec.CommandContext(ctx, c.Bin, "pr", "checks", fmt.Sprint(number), "--watch", "--fail-fast")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := string(out)

	if ctx.Err() != nil {
		return ChecksFail, text, ctx.Err()
	}
	if err == nil {
		return ChecksPass, text, nil
	}
	// gh phrases this as "no checks reported on the <sha> commit".
	if strings.Contains(strings.ToLower(text), "no checks reported") {
		return ChecksNone, text, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == ghChecksPendingExit {
		return ChecksPending, text, nil
	}
	return ChecksFail, text, nil
}

// FailingCheck is one red check, as handed to the Codex fix session.
type FailingCheck struct {
	Name        string `json:"name"`
	Bucket      string `json:"bucket"`
	Workflow    string `json:"workflow"`
	Link        string `json:"link"`
	Description string `json:"description"`
}

// FailingChecks lists the checks that failed or were cancelled.
func (c *Client) FailingChecks(ctx context.Context, dir string, number int) ([]FailingCheck, error) {
	out, err := c.runIn(ctx, dir, "pr", "checks", fmt.Sprint(number),
		"--json", "name,bucket,workflow,link,description")
	// gh exits nonzero when checks are red, which is precisely the case this is
	// called in, so the output matters more than the status.
	if len(bytes.TrimSpace(out)) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	var all []FailingCheck
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("gh pr checks --json: %w", err)
	}
	var failed []FailingCheck
	for _, ch := range all {
		if ch.Bucket == "fail" || ch.Bucket == "cancel" {
			failed = append(failed, ch)
		}
	}
	return failed, nil
}

// runIn is run with an explicit working directory, so gh resolves the
// repository from the checkout rather than from wherever quorum was started.
func (c *Client) runIn(ctx context.Context, dir string, args ...string) ([]byte, error) {
	attempts := max(c.Attempts, 1)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
		cmd := exec.CommandContext(runCtx, c.Bin, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()

		if err == nil {
			return stdout.Bytes(), nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			return stdout.Bytes(), fmt.Errorf("gh %s: %w", args[0], ctx.Err())
		}
		lastErr = fmt.Errorf("gh %s: %s", strings.Join(args, " "), firstLine(msg))
		if !looksTransient(msg) {
			return stdout.Bytes(), lastErr
		}
		if attempt < attempts {
			wait := time.Duration(attempt) * c.Backoff
			c.logf("gh %s failed (%s), retrying in %s", args[0], firstLine(msg), wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("%w: %v", ErrTransient, lastErr)
}

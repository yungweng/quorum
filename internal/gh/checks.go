package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
)

// The three buckets a single check or workflow run can be in.
const (
	BucketPass    = "pass"
	BucketPending = "pending"
	BucketFail    = "fail"
)

// CommitCheck is one check run or commit status reported for an exact commit.
// When a name was reported more than once (a rerun), only the newest counts,
// which is also what GitHub's merge box shows.
type CommitCheck struct {
	Name   string
	Bucket string
}

// CommitChecks reports every check run and commit status of one exact commit.
//
// Unlike `gh pr checks`, which answers for whatever GitHub currently takes to
// be the PR head, this is bound to the sha it is asked about.
func (c *Client) CommitChecks(ctx context.Context, repo, sha string) ([]CommitCheck, error) {
	out, err := c.run(ctx, "api", "--paginate",
		fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100&filter=latest", repo, sha))
	if err != nil {
		return nil, err
	}
	type checkRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	newest := map[string]checkRun{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("gh api check runs: %w", err)
		}
		for _, run := range page.CheckRuns {
			if prev, ok := newest[run.Name]; !ok || run.ID > prev.ID {
				newest[run.Name] = run
			}
		}
	}
	var checks []CommitCheck
	for _, run := range newest {
		checks = append(checks, CommitCheck{Name: run.Name, Bucket: runBucket(run.Status, run.Conclusion)})
	}

	// The combined status already holds only the newest status per context.
	out, err = c.run(ctx, "api", fmt.Sprintf("repos/%s/commits/%s/status?per_page=100", repo, sha))
	if err != nil {
		return nil, err
	}
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(out, &combined); err != nil {
		return nil, fmt.Errorf("gh api combined status: %w", err)
	}
	for _, st := range combined.Statuses {
		bucket := BucketFail
		switch st.State {
		case "success":
			bucket = BucketPass
		case "pending":
			bucket = BucketPending
		}
		checks = append(checks, CommitCheck{Name: st.Context, Bucket: bucket})
	}
	slices.SortFunc(checks, func(a, b CommitCheck) int { return strings.Compare(a.Name, b.Name) })
	return checks, nil
}

// WorkflowRun is one GitHub Actions run of an exact commit.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	WorkflowID int64  `json:"workflow_id"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
}

// Bucket classifies the run the way a check is classified.
func (w WorkflowRun) Bucket() string { return runBucket(w.Status, w.Conclusion) }

// WorkflowRuns lists the Actions runs of one exact commit. A workflow that ran
// more than once for the same event (an edited PR, a re-push of the same sha)
// is represented by its newest run only, because that is the one whose checks
// GitHub shows.
//
// This is what sees a run that failed before it started a single job, such as
// a workflow file GitHub could not parse: such a run reports no check at all,
// so a check list alone cannot tell it from a run that never existed.
func (c *Client) WorkflowRuns(ctx context.Context, repo, sha string) ([]WorkflowRun, error) {
	out, err := c.run(ctx, "api", "--paginate",
		fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", repo, url.QueryEscape(sha)))
	if err != nil {
		return nil, err
	}
	type key struct {
		workflow int64
		event    string
	}
	newest := map[key]WorkflowRun{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page struct {
			WorkflowRuns []WorkflowRun `json:"workflow_runs"`
		}
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("gh api workflow runs: %w", err)
		}
		for _, run := range page.WorkflowRuns {
			k := key{run.WorkflowID, run.Event}
			if prev, ok := newest[k]; !ok || run.ID > prev.ID {
				newest[k] = run
			}
		}
	}
	runs := make([]WorkflowRun, 0, len(newest))
	for _, run := range newest {
		runs = append(runs, run)
	}
	slices.SortFunc(runs, func(a, b WorkflowRun) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(a.Event, b.Event)
	})
	return runs, nil
}

// HasActiveWorkflows reports whether the repository has at least one enabled
// workflow file. A repository without one never gets an Actions run, so there
// is nothing to wait for.
func (c *Client) HasActiveWorkflows(ctx context.Context, repo string) (bool, error) {
	out, err := c.run(ctx, "api", "--paginate", fmt.Sprintf("repos/%s/actions/workflows?per_page=100", repo))
	if err != nil {
		return false, err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page struct {
			Workflows []struct {
				Path  string `json:"path"`
				State string `json:"state"`
			} `json:"workflows"`
		}
		if err := dec.Decode(&page); err == io.EOF {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("gh api workflows: %w", err)
		}
		for _, wf := range page.Workflows {
			// Dynamic workflows (code scanning default setup, Dependabot) have
			// no file and are not started by a pushed pull request head.
			if wf.State == "active" && strings.HasPrefix(wf.Path, ".github/workflows/") {
				return true, nil
			}
		}
	}
}

// RequiredChecks lists the status check contexts the base branch requires,
// from classic branch protection and from rulesets together. These are the
// checks GitHub waits for before it allows a merge, including ones that have
// not been reported yet - which is exactly what the PR's own check list
// cannot show.
func (c *Client) RequiredChecks(ctx context.Context, repo, branch string) ([]string, error) {
	escaped := url.PathEscape(branch)
	out, err := c.run(ctx, "api", fmt.Sprintf("repos/%s/branches/%s", repo, escaped))
	if err != nil {
		return nil, err
	}
	var br struct {
		Protection struct {
			RequiredStatusChecks struct {
				EnforcementLevel string   `json:"enforcement_level"`
				Contexts         []string `json:"contexts"`
				Checks           []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := json.Unmarshal(out, &br); err != nil {
		return nil, fmt.Errorf("gh api branch: %w", err)
	}
	seen := map[string]bool{}
	var required []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			required = append(required, name)
		}
	}
	if rsc := br.Protection.RequiredStatusChecks; rsc.EnforcementLevel != "off" {
		for _, name := range rsc.Contexts {
			add(name)
		}
		for _, ch := range rsc.Checks {
			add(ch.Context)
		}
	}

	out, err = c.run(ctx, "api", "--paginate", fmt.Sprintf("repos/%s/rules/branches/%s?per_page=100", repo, escaped))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}
		if err := dec.Decode(&rules); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("gh api branch rules: %w", err)
		}
		for _, rule := range rules {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, ch := range rule.Parameters.RequiredStatusChecks {
				add(ch.Context)
			}
		}
	}
	slices.Sort(required)
	return required, nil
}

// runBucket maps a check run's or workflow run's status and conclusion onto
// pass, pending or fail. Skipped and neutral count as passing, as they do for
// GitHub's required checks; everything that did not succeed fails.
func runBucket(status, conclusion string) string {
	if status != "completed" {
		return BucketPending
	}
	switch conclusion {
	case "success", "skipped", "neutral":
		return BucketPass
	default:
		return BucketFail
	}
}

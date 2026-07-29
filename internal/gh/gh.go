// Package gh wraps the GitHub CLI.
//
// Every call goes through one retry policy. That matters more than it sounds:
// the failures seen in practice are TLS handshake timeouts, a token refresh
// racing a request into an HTTP 401, and GitHub's own search occasionally
// rejecting a query it accepted a minute earlier. None of those mean "there is
// nothing to review", and the caller must be able to tell the difference.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client runs gh. The zero value is unusable; use New.
type Client struct {
	Bin      string
	Attempts int
	Backoff  time.Duration
	Timeout  time.Duration
	// Log receives one line per retry. Nil discards them.
	Log func(format string, args ...any)
}

func New(bin string) *Client {
	return &Client{Bin: bin, Attempts: 3, Backoff: 2 * time.Second, Timeout: 120 * time.Second}
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

// ErrTransient marks a failure that is worth trying again later. Callers use it
// to keep a pull request in the queue instead of recording a decision about it.
var ErrTransient = errors.New("temporary GitHub failure")

// transientSigns are matched against gh's stderr. They are deliberately narrow:
// a genuine "not found" or "no permission" must not look retryable.
var transientSigns = []string{
	"timeout",
	"timed out",
	"tls handshake",
	"connection reset",
	"connection refused",
	"no such host",
	"unexpected eof",
	"i/o timeout",
	"temporary failure",
	"502",
	"503",
	"504",
	"bad gateway",
	"service unavailable",
	"server error",
	"rate limit",
	"http 401", // a token refresh racing a request, not a logged-out gh
	// GitHub search intermittently rejects review-requested:@me with this.
	"cannot be searched",
}

func looksTransient(s string) bool {
	low := strings.ToLower(s)
	for _, sign := range transientSigns {
		if strings.Contains(low, sign) {
			return true
		}
	}
	return false
}

// run executes gh and returns stdout. A transient failure is retried with a
// linear backoff and finally reported wrapped in ErrTransient.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	attempts := max(c.Attempts, 1)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
		cmd := exec.CommandContext(runCtx, c.Bin, args...)
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
		// A killed context is the caller shutting us down, not a GitHub problem.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("gh %s: %w", args[0], ctx.Err())
		}
		// cancel() has already run, so runCtx.Err() is non-nil either way. Only
		// DeadlineExceeded means the command really ran out of time; Canceled
		// just means it finished first.
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			msg = "timed out after " + c.Timeout.String()
		}
		lastErr = fmt.Errorf("gh %s: %s", strings.Join(args, " "), firstLine(msg))

		if !looksTransient(msg) {
			return nil, lastErr
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// PR is one search result.
type PR struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	IsDraft    bool   `json:"isDraft"`
	UpdatedAt  string `json:"updatedAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
}

// Key is the "owner/repo#number" identity used everywhere else.
func (p PR) Key() string { return fmt.Sprintf("%s#%d", p.Repository.NameWithOwner, p.Number) }

const searchFields = "number,repository,title,author,updatedAt,isDraft,url"

// Login is the authenticated user's handle.
func (c *Client) Login(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(string(out))
	if login == "" {
		return "", fmt.Errorf("%w: gh returned no login", ErrTransient)
	}
	return login, nil
}

// Teams lists "org/slug" for every team the user belongs to.
func (c *Client) Teams(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "api", "user/teams", "--paginate",
		"--jq", `.[] | "\(.organization.login)/\(.slug)"`)
	if err != nil {
		return nil, err
	}
	var teams []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			teams = append(teams, line)
		}
	}
	return teams, nil
}

// SearchReviewRequested returns open pull requests that ask the authenticated
// user for a review. scope holds extra qualifiers such as --owner=acme.
func (c *Client) SearchReviewRequested(ctx context.Context, scope []string) ([]PR, error) {
	args := append([]string{"search", "prs", "--review-requested=@me", "--state=open", "--limit=100"}, scope...)
	return c.searchPRs(ctx, append(args, "--json", searchFields))
}

// SearchTeamReviewRequested returns pull requests that ask a team for a review.
// GitHub's review-requested: qualifier does not cover those despite the docs,
// so each team needs its own search.
func (c *Client) SearchTeamReviewRequested(ctx context.Context, team string, scope []string) ([]PR, error) {
	args := append([]string{"search", "prs", "team-review-requested:" + team, "--state=open", "--limit=100"}, scope...)
	return c.searchPRs(ctx, append(args, "--json", searchFields))
}

func (c *Client) searchPRs(ctx context.Context, args []string) ([]PR, error) {
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	// An empty body is not an empty result: gh prints "[]" when it found
	// nothing, so nothing at all means the call did not really succeed.
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("%w: gh search produced no output", ErrTransient)
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh search: %w", err)
	}
	return prs, nil
}

// Details are the fields the search API does not carry.
type Details struct {
	HeadRefOid        string `json:"headRefOid"`
	IsDraft           bool   `json:"isDraft"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	State             string `json:"state"`
	Title             string `json:"title"`
	Author            struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
}

// PRDetails fetches the head SHA and the fork flag for one pull request.
func (c *Client) PRDetails(ctx context.Context, repo string, number int) (Details, error) {
	var d Details
	out, err := c.run(ctx, "pr", "view", fmt.Sprint(number), "--repo", repo,
		"--json", "headRefOid,isDraft,isCrossRepository,author,state,title")
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return d, fmt.Errorf("gh pr view %s#%d: %w", repo, number, err)
	}
	if d.HeadRefOid == "" {
		return d, fmt.Errorf("%w: gh pr view %s#%d returned no head sha", ErrTransient, repo, number)
	}
	return d, nil
}

// The states a pull request can be in, as GitHub spells them.
const (
	StateOpen   = "OPEN"
	StateClosed = "CLOSED"
	StateMerged = "MERGED"
)

// safeRepo matches the owner/name shapes GitHub actually allows. Repository
// names reach PRStates from the state file and are interpolated into a query,
// so anything else is dropped rather than quoted and hoped for.
var safeRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// PRStates reports whether each pull request is still open, closed or merged.
//
// It takes "owner/repo#number" keys and answers in one GraphQL round trip
// whatever the number of pull requests, because the caller is a screen that
// redraws: one request per visible line would turn a dashboard into a source of
// rate limiting. Keys it cannot parse are left out of the result rather than
// reported, since a caller that only decorates its output cannot do anything
// about them.
func (c *Client) PRStates(ctx context.Context, keys []string) (map[string]string, error) {
	type target struct {
		alias       string
		key         string
		owner, name string
		number      int
	}
	var targets []target
	var q strings.Builder
	q.WriteString("query {")
	for _, key := range keys {
		repo, numText, ok := strings.Cut(key, "#")
		if !ok || !safeRepo.MatchString(repo) {
			continue
		}
		number, err := strconv.Atoi(numText)
		if err != nil || number <= 0 {
			continue
		}
		owner, name, _ := strings.Cut(repo, "/")
		alias := fmt.Sprintf("p%d", len(targets))
		targets = append(targets, target{alias, key, owner, name, number})
		fmt.Fprintf(&q, " %s: repository(owner: %q, name: %q) { pullRequest(number: %d) { state } }",
			alias, owner, name, number)
	}
	q.WriteString(" }")
	if len(targets) == 0 {
		return map[string]string{}, nil
	}

	out, err := c.run(ctx, "api", "graphql", "-f", "query="+q.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data map[string]*struct {
			PullRequest *struct {
				State string `json:"state"`
			} `json:"pullRequest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	states := make(map[string]string, len(targets))
	for _, t := range targets {
		// A repository that has gone away answers null, which is not an error
		// worth failing the whole batch over: the other pull requests are fine.
		if node := resp.Data[t.alias]; node != nil && node.PullRequest != nil {
			states[t.key] = node.PullRequest.State
		}
	}
	return states, nil
}

// timelineEvent is the subset of the timeline API this needs.
type timelineEvent struct {
	Event             string `json:"event"`
	CreatedAt         string `json:"created_at"`
	RequestedReviewer *struct {
		Login string `json:"login"`
	} `json:"requested_reviewer"`
	RequestedTeam *struct {
		Slug string `json:"slug"`
	} `json:"requested_team"`
}

// LatestReviewRequest returns the timestamp of the most recent review request
// aimed at login or at one of teamSlugs, or "" when there is none.
//
// This timestamp is the trigger: a request quorum has not recorded yet means
// review now, and re-requesting a review produces a fresh one on purpose.
func (c *Client) LatestReviewRequest(ctx context.Context, repo string, number int, login string, teamSlugs []string) (string, error) {
	out, err := c.run(ctx, "api", fmt.Sprintf("repos/%s/issues/%d/timeline", repo, number), "--paginate")
	if err != nil {
		return "", err
	}
	wanted := map[string]bool{}
	for _, s := range teamSlugs {
		wanted[s] = true
	}

	// gh merges paginated arrays into one, but has not always done so. Decoding
	// successive top-level values reads either shape.
	dec := json.NewDecoder(bytes.NewReader(out))
	latest := ""
	for {
		var page []timelineEvent
		if err := dec.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("timeline %s#%d: %w", repo, number, err)
		}
		for _, e := range page {
			if e.Event != "review_requested" {
				continue
			}
			mine := e.RequestedReviewer != nil && e.RequestedReviewer.Login == login
			ours := e.RequestedTeam != nil && wanted[e.RequestedTeam.Slug]
			if (mine || ours) && e.CreatedAt > latest {
				latest = e.CreatedAt
			}
		}
	}
	return latest, nil
}

// TeamSlugsFor returns the slugs of teams that belong to the given owner, since
// a team can only request a review inside its own organisation.
func TeamSlugsFor(owner string, teams []string) []string {
	var out []string
	for _, t := range teams {
		org, slug, ok := strings.Cut(t, "/")
		if ok && org == owner {
			out = append(out, slug)
		}
	}
	return out
}

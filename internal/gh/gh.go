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
	"regexp"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/firstline"
	"github.com/yungweng/quorum/internal/prkey"
	"github.com/yungweng/quorum/internal/proc"
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

// NewDecoration returns a client for lookups that only decorate a screen: one
// attempt, the given deadline, and no retry log, which would echo onto the
// screen being drawn.
func NewDecoration(bin string, timeout time.Duration) *Client {
	c := New(bin)
	c.Attempts = 1
	c.Timeout = timeout
	return c
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

// run executes gh in dir ("" for the process's own directory, otherwise gh
// resolves the repository from that checkout) and returns stdout.
func (c *Client) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return c.exec(ctx, invocation{dir: dir}, args...)
}

// invocation is how one gh command runs, beyond its arguments.
type invocation struct {
	dir   string
	stdin []byte
	// noTimeoutRetry does not retry a command killed by the per-call timeout.
	// A write that timed out may still have reached GitHub.
	noTimeoutRetry bool
	// write marks a request that must not reach GitHub twice, such as posting
	// a comment. It is retried only when the failure shows that it never left
	// this machine; a 502 or a reset connection can follow a request GitHub
	// accepted, and repeating it posts the comment again. It implies
	// noTimeoutRetry.
	write bool
}

// unsentSigns are the transient failures that happen before a request is on
// the wire, which makes repeating even a write safe.
var unsentSigns = []string{
	"no such host",
	"connection refused",
	"tls handshake",
	"temporary failure", // in name resolution
}

func neverSent(msg string) bool {
	low := strings.ToLower(msg)
	for _, sign := range unsentSigns {
		if strings.Contains(low, sign) {
			return true
		}
	}
	return false
}

// RetryLogPhrase is in every log line of a retried gh command, which is what
// doctor counts.
const RetryLogPhrase = "retrying in"

// exec runs gh and returns stdout. gh runs in its own process group, so the
// per-call timeout and a cancelled ctx reach its children as well, a
// credential helper or the git below `gh repo clone`. A transient failure is
// retried with a linear backoff and finally reported wrapped in ErrTransient. A failed command
// may still return stdout: GraphQL uses partial JSON responses when one field
// fails, and callers that understand those responses must be able to inspect
// them. All other callers discard stdout when err is non-nil.
func (c *Client) exec(ctx context.Context, inv invocation, args ...string) ([]byte, error) {
	attempts := max(c.Attempts, 1)

	var lastErr error
	var lastOut []byte
	for attempt := 1; attempt <= attempts; attempt++ {
		var stdout, stderr bytes.Buffer
		spec := proc.Spec{Name: c.Bin, Args: args, Dir: inv.dir, Stdout: &stdout, Stderr: &stderr}
		if inv.stdin != nil {
			spec.Stdin = bytes.NewReader(inv.stdin)
		}
		err := proc.Run(ctx, c.Timeout, spec)
		lastOut = bytes.Clone(stdout.Bytes())

		if err == nil {
			return lastOut, nil
		}

		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// A killed context is the caller shutting us down, not a GitHub problem.
		if ctx.Err() != nil {
			return lastOut, fmt.Errorf("gh %s: %w", args[0], ctx.Err())
		}
		timedOut := errors.Is(err, proc.ErrTimeout)
		if timedOut {
			msg = "timed out after " + c.Timeout.String()
		}
		lastErr = fmt.Errorf("gh %s: %s", strings.Join(args, " "), firstline.Trimmed(msg))

		if timedOut && (inv.noTimeoutRetry || inv.write) {
			return lastOut, lastErr
		}
		if !looksTransient(msg) {
			return lastOut, lastErr
		}
		if inv.write && !neverSent(msg) {
			return lastOut, fmt.Errorf("%w: %v", ErrTransient, lastErr)
		}
		if attempt < attempts {
			wait := time.Duration(attempt) * c.Backoff
			c.logf("gh %s failed (%s), "+RetryLogPhrase+" %s", args[0], firstline.Trimmed(msg), wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return lastOut, fmt.Errorf("%w: %v", ErrTransient, lastErr)
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
func (p PR) Key() string { return prkey.Format(p.Repository.NameWithOwner, p.Number) }

const searchFields = "number,repository,title,author,updatedAt,isDraft,url"

// Login is the authenticated user's handle.
func (c *Client) Login(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "", "api", "user", "--jq", ".login")
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
	out, err := c.run(ctx, "", "api", "user/teams", "--paginate",
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
	out, err := c.run(ctx, "", args...)
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

type LatestReview struct {
	State  string `json:"state"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

// Details are the fields the search API does not carry.
type Details struct {
	BaseRefName       string         `json:"baseRefName"`
	HeadRefOid        string         `json:"headRefOid"`
	IsDraft           bool           `json:"isDraft"`
	IsCrossRepository bool           `json:"isCrossRepository"`
	State             string         `json:"state"`
	Title             string         `json:"title"`
	Mergeable         string         `json:"mergeable"`
	MergeStateStatus  string         `json:"mergeStateStatus"`
	ReviewDecision    string         `json:"reviewDecision"`
	LatestReviews     []LatestReview `json:"latestReviews"`
	AutoMergeRequest  *struct {
		EnabledAt string `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	Author struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
}

func (c *Client) PRDetails(ctx context.Context, repo string, number int) (Details, error) {
	var d Details
	out, err := c.run(ctx, "", "pr", "view", fmt.Sprint(number), "--repo", repo,
		"--json", "baseRefName,headRefOid,isDraft,isCrossRepository,author,state,title,mergeable,mergeStateStatus,reviewDecision,latestReviews,autoMergeRequest")
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

// QueueEntry is one pull request's place in its target branch's merge queue.
// HeadOid is what makes it comparable to a reviewed head: an entry for another
// commit belongs to a push that happened after the review.
type QueueEntry struct {
	State    string
	Position int
	HeadOid  string
}

// MergeSettings are the repository and branch rules that select a safe merge
// path. A repository can allow more than one method. PullRequestID is the node
// id EnqueueHead needs, and QueueEntry is non-nil only while the pull request
// already sits in the queue.
type MergeSettings struct {
	QueueEnabled       bool
	MergeCommitAllowed bool
	SquashMergeAllowed bool
	RebaseMergeAllowed bool
	PullRequestID      string
	QueueEntry         *QueueEntry
}

// MergePolicy reports the repository settings that select a safe merge path
// and confirms that the queried pull request still has headSHA.
//
// The merge queue entry is read here rather than in PRDetails because
// `gh pr view --json` exposes no mergeQueueEntry field; GraphQL is the only
// way to ask, and this call already selects the pull request node.
func (c *Client) MergePolicy(ctx context.Context, repo string, number int, headSHA string) (MergeSettings, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || number <= 0 || headSHA == "" {
		return MergeSettings{}, fmt.Errorf("merge policy check needs owner/repository, pull request number, and head sha")
	}
	const query = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    mergeCommitAllowed
    squashMergeAllowed
    rebaseMergeAllowed
    pullRequest(number: $number) {
      id
      headRefOid
      isMergeQueueEnabled
      mergeQueueEntry { state position headCommit { oid } }
    }
  }
}`
	out, err := c.run(ctx, "", "api", "graphql", "-f", "query="+query,
		"-F", "owner="+owner, "-F", "name="+name, "-F", "number="+fmt.Sprint(number))
	if err != nil {
		return MergeSettings{}, err
	}
	var response struct {
		Data struct {
			Repository struct {
				MergeCommitAllowed bool `json:"mergeCommitAllowed"`
				SquashMergeAllowed bool `json:"squashMergeAllowed"`
				RebaseMergeAllowed bool `json:"rebaseMergeAllowed"`
				PullRequest        *struct {
					ID                string          `json:"id"`
					HeadRefOid        string          `json:"headRefOid"`
					MergeQueueEnabled bool            `json:"isMergeQueueEnabled"`
					MergeQueueEntry   *queueEntryNode `json:"mergeQueueEntry"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return MergeSettings{}, fmt.Errorf("gh api merge policy check: %w", err)
	}
	pr := response.Data.Repository.PullRequest
	if pr == nil || pr.HeadRefOid == "" {
		return MergeSettings{}, fmt.Errorf("%w: GitHub returned no pull request head during merge policy check", ErrTransient)
	}
	if pr.HeadRefOid != headSHA {
		return MergeSettings{}, fmt.Errorf("pull request head moved from %s to %s during merge policy check", headSHA, pr.HeadRefOid)
	}
	return MergeSettings{
		QueueEnabled:       pr.MergeQueueEnabled,
		MergeCommitAllowed: response.Data.Repository.MergeCommitAllowed,
		SquashMergeAllowed: response.Data.Repository.SquashMergeAllowed,
		RebaseMergeAllowed: response.Data.Repository.RebaseMergeAllowed,
		PullRequestID:      pr.ID,
		QueueEntry:         pr.MergeQueueEntry.entry(),
	}, nil
}

// queueEntryNode is the GraphQL shape of a MergeQueueEntry. Both MergePolicy
// and EnqueueHead decode the same selection.
type queueEntryNode struct {
	State      string `json:"state"`
	Position   int    `json:"position"`
	HeadCommit *struct {
		Oid string `json:"oid"`
	} `json:"headCommit"`
}

func (n *queueEntryNode) entry() *QueueEntry {
	if n == nil {
		return nil
	}
	entry := QueueEntry{State: n.State, Position: n.Position}
	if n.HeadCommit != nil {
		entry.HeadOid = n.HeadCommit.Oid
	}
	return &entry
}

// Pull request states returned to dashboard callers. The first four are
// GitHub states; UNAVAILABLE records a missing repository or pull request.
const (
	StateOpen        = "OPEN"
	StateAutoMerge   = "AUTO_MERGE"
	StateClosed      = "CLOSED"
	StateMerged      = "MERGED"
	StateUnavailable = "UNAVAILABLE"
)

// ValidPRState reports whether state is one PRStates can answer.
func ValidPRState(state string) bool {
	switch state {
	case StateOpen, StateAutoMerge, StateClosed, StateMerged, StateUnavailable:
		return true
	default:
		return false
	}
}

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
		repo, number := prkey.Repo(key), prkey.Number(key)
		if !safeRepo.MatchString(repo) || number <= 0 {
			continue
		}
		owner, name, _ := strings.Cut(repo, "/")
		alias := fmt.Sprintf("p%d", len(targets))
		targets = append(targets, target{alias, key, owner, name, number})
		fmt.Fprintf(&q, " %s: repository(owner: %q, name: %q) { pullRequest(number: %d) { state autoMergeRequest { enabledAt } mergeQueueEntry { position } } }",
			alias, owner, name, number)
	}
	q.WriteString(" }")
	if len(targets) == 0 {
		return map[string]string{}, nil
	}

	out, runErr := c.run(ctx, "", "api", "graphql", "-f", "query="+q.String())
	var resp struct {
		Data map[string]*struct {
			PullRequest *struct {
				State            string          `json:"state"`
				AutoMergeRequest json.RawMessage `json:"autoMergeRequest"`
				MergeQueueEntry  json.RawMessage `json:"mergeQueueEntry"`
			} `json:"pullRequest"`
		} `json:"data"`
		Errors []struct {
			Type    string   `json:"type"`
			Path    []string `json:"path"`
			Message string   `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	aliases := make(map[string]bool, len(targets))
	for _, t := range targets {
		aliases[t.alias] = true
	}
	for _, graphErr := range resp.Errors {
		if graphErr.Type != "NOT_FOUND" || len(graphErr.Path) == 0 || !aliases[graphErr.Path[0]] {
			if runErr != nil {
				return nil, runErr
			}
			return nil, fmt.Errorf("gh api graphql: %s", graphErr.Message)
		}
		// Tolerated: the repository is gone (path names only the alias and its
		// data is null) or the pull request number does not exist in it (path
		// descends into pullRequest). Both leave a null node that the loop
		// below records as unavailable. One stale key must not freeze the
		// merge states of every other tracked pull request.
		missingRepo := len(graphErr.Path) == 1 && resp.Data[graphErr.Path[0]] == nil
		missingPR := len(graphErr.Path) == 2 && graphErr.Path[1] == "pullRequest"
		if !missingRepo && !missingPR {
			if runErr != nil {
				return nil, runErr
			}
			return nil, fmt.Errorf("gh api graphql: %s", graphErr.Message)
		}
	}
	if runErr != nil && len(resp.Errors) == 0 {
		return nil, runErr
	}
	states := make(map[string]string, len(targets))
	for _, t := range targets {
		// A deleted or inaccessible repository answers null. Record that fact so
		// the dashboard drops a stale OPEN value instead of checking forever.
		node := resp.Data[t.alias]
		if node == nil || node.PullRequest == nil {
			states[t.key] = StateUnavailable
			continue
		}
		state := node.PullRequest.State
		if state == StateOpen && (present(node.PullRequest.AutoMergeRequest) || present(node.PullRequest.MergeQueueEntry)) {
			state = StateAutoMerge
		}
		states[t.key] = state
	}
	return states, nil
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
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
	out, err := c.run(ctx, "", "api", fmt.Sprintf("repos/%s/issues/%d/timeline", repo, number), "--paginate")
	if err != nil {
		return "", err
	}
	wanted := map[string]bool{}
	for _, s := range teamSlugs {
		wanted[s] = true
	}

	latest := ""
	if err := decodePages(out, func(page []timelineEvent) bool {
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
		return true
	}); err != nil {
		return "", fmt.Errorf("timeline %s#%d: %w", repo, number, err)
	}
	return latest, nil
}

// decodePages hands each page of a --paginate response to page until page
// returns false or the output ends.
//
// gh merges paginated arrays into one, but has not always done so, and object
// responses arrive as one top-level value per page. Decoding successive
// top-level values reads every shape.
func decodePages[T any](out []byte, page func(T) bool) error {
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var v T
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if !page(v) {
			return nil
		}
	}
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

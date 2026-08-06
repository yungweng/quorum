package gh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGH writes a script that stands in for the gh binary. The script prints
// the given stdout and stderr and exits with code, counting its invocations in
// a file so a test can assert how often it was retried.
func fakeGH(t *testing.T, body string) (bin, countFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "gh")
	countFile = filepath.Join(dir, "count")
	script := "#!/bin/bash\n" +
		"n=$(cat " + countFile + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1)); echo $n > " + countFile + "\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, countFile
}

func calls(t *testing.T, countFile string) int {
	t.Helper()
	b, err := os.ReadFile(countFile)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func testClient(bin string) *Client {
	c := New(bin)
	c.Backoff = time.Millisecond
	c.Timeout = 10 * time.Second
	return c
}

func TestLooksTransient(t *testing.T) {
	transient := []string{
		`Get "https://api.github.com/repos/x/y/issues/1/timeline": net/http: TLS handshake timeout`,
		"gh: Requires authentication (HTTP 401)",
		`Invalid search query "review-requested:@me state:open type:pr".
The listed users cannot be searched either because the users do not exist or you do not have permission to view the users.`,
		"HTTP 503: Service Unavailable",
		"dial tcp: lookup api.github.com: no such host",
		"You have exceeded a secondary rate limit",
	}
	for _, msg := range transient {
		if !looksTransient(msg) {
			t.Errorf("should be retried: %q", msg)
		}
	}
	permanent := []string{
		"gh: Not Found (HTTP 404)",
		"GraphQL: Could not resolve to a Repository with the name 'acme/nope'. (repository)",
		"unknown flag: --nope",
		"gh: Must have admin rights to Repository. (HTTP 403)",
	}
	for _, msg := range permanent {
		if looksTransient(msg) {
			t.Errorf("should not be retried: %q", msg)
		}
	}
}

func TestRunRetriesTransientAndSucceeds(t *testing.T) {
	bin, countFile := fakeGH(t, `
if [ "$n" -lt 3 ]; then
  echo "net/http: TLS handshake timeout" >&2
  exit 1
fi
echo -n "yungweng"`)
	out, err := testClient(bin).run(context.Background(), "api", "user")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(out) != "yungweng" {
		t.Errorf("stdout = %q", out)
	}
	if got := calls(t, countFile); got != 3 {
		t.Errorf("gh called %d times, want 3", got)
	}
}

func TestRunGivesUpAsTransient(t *testing.T) {
	bin, countFile := fakeGH(t, `echo "net/http: TLS handshake timeout" >&2; exit 1`)
	_, err := testClient(bin).run(context.Background(), "api", "user")
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
	if got := calls(t, countFile); got != 3 {
		t.Errorf("gh called %d times, want 3 attempts", got)
	}
}

func TestRunDoesNotRetryPermanentFailure(t *testing.T) {
	bin, countFile := fakeGH(t, `echo "gh: Not Found (HTTP 404)" >&2; exit 1`)
	_, err := testClient(bin).run(context.Background(), "pr", "view", "9999")
	if err == nil {
		t.Fatal("a 404 was reported as success")
	}
	if errors.Is(err, ErrTransient) {
		t.Error("a 404 was classified as temporary")
	}
	if got := calls(t, countFile); got != 1 {
		t.Errorf("gh called %d times, want 1", got)
	}
}

func TestApproveDoesNotRetryAmbiguousPost(t *testing.T) {
	bin, countFile := fakeGH(t, `echo "net/http: TLS handshake timeout" >&2; exit 1`)
	_, err := testClient(bin).ApproveHead(context.Background(), "acme/api", 42, "abc123", "approved")
	if err == nil {
		t.Fatal("a timed-out approval was reported as success")
	}
	if got := calls(t, countFile); got != 1 {
		t.Fatalf("approval POST ran %d times, want one", got)
	}
}

func TestApproveReturnsReviewID(t *testing.T) {
	bin, _ := fakeGH(t, `echo '{"id":99,"state":"APPROVED"}'`)
	review, err := testClient(bin).ApproveHead(context.Background(), "acme/api", 42, "abc123", "approved")
	if err != nil {
		t.Fatal(err)
	}
	if review.ID != 99 || review.State != "APPROVED" {
		t.Fatalf("review = %+v", review)
	}
}

func TestMergeHeadRejectsUnmergedResponse(t *testing.T) {
	bin, _ := fakeGH(t, `echo '{"merged":false,"message":"Base branch was modified"}'`)
	err := testClient(bin).MergeHead(context.Background(), "acme/api", 42, "abc123", MergeMethodMerge)
	if err == nil || !strings.Contains(err.Error(), "Base branch was modified") {
		t.Fatalf("err = %v", err)
	}
}

func TestMergeHeadUsesSelectedMethod(t *testing.T) {
	for _, method := range []MergeMethod{MergeMethodMerge, MergeMethodSquash, MergeMethodRebase} {
		t.Run(string(method), func(t *testing.T) {
			bin, _ := fakeGH(t, `
if [[ "$*" != *"merge_method=`+string(method)+`"* || "$*" != *"sha=abc123"* ]]; then
  echo "unexpected arguments: $*" >&2
  exit 1
fi
echo '{"merged":true}'`)
			if err := testClient(bin).MergeHead(context.Background(), "acme/api", 42, "abc123", method); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMergeHeadRejectsUnknownMethodBeforeCallingGitHub(t *testing.T) {
	bin, countFile := fakeGH(t, `echo '{"merged":true}'`)
	err := testClient(bin).MergeHead(context.Background(), "acme/api", 42, "abc123", MergeMethod("octopus"))
	if err == nil || !strings.Contains(err.Error(), "unsupported merge method") {
		t.Fatalf("err = %v", err)
	}
	if got := calls(t, countFile); got != 0 {
		t.Fatalf("gh called %d times, want none", got)
	}
}

func TestMergePolicyChecksTheExactHead(t *testing.T) {
	bin, _ := fakeGH(t, `
if [[ "$*" != *"isMergeQueueEnabled"* || "$*" != *"mergeCommitAllowed"* || "$*" != *"squashMergeAllowed"* || "$*" != *"rebaseMergeAllowed"* || "$*" != *"owner=acme"* || "$*" != *"name=api"* || "$*" != *"number=42"* ]]; then
  echo "unexpected arguments: $*" >&2
  exit 1
fi
echo '{"data":{"repository":{"mergeCommitAllowed":true,"squashMergeAllowed":true,"rebaseMergeAllowed":false,"pullRequest":{"headRefOid":"abc123","isMergeQueueEnabled":true}}}}'`)
	settings, err := testClient(bin).MergePolicy(context.Background(), "acme/api", 42, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !settings.QueueEnabled {
		t.Fatal("required merge queue was not detected")
	}
	if !settings.MergeCommitAllowed || !settings.SquashMergeAllowed || settings.RebaseMergeAllowed {
		t.Fatalf("merge settings = %+v", settings)
	}
}

func TestWatchChecksClassifiesTransientFailure(t *testing.T) {
	bin, _ := fakeGH(t, `echo "net/http: TLS handshake timeout" >&2; exit 1`)
	state, _, err := testClient(bin).WatchChecks(context.Background(), t.TempDir(), 42)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
	if state != ChecksPending {
		t.Fatalf("state = %v, want ChecksPending", state)
	}
}

func TestWatchRequiredChecksFiltersOptionalChecks(t *testing.T) {
	bin, _ := fakeGH(t, `
if [[ "$*" != "pr checks 42 --watch --fail-fast --required" ]]; then
  echo "unexpected arguments: $*" >&2
  exit 1
fi
echo 'required checks passed'`)
	state, _, err := testClient(bin).WatchRequiredChecks(context.Background(), t.TempDir(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if state != ChecksPass {
		t.Fatalf("state = %v, want ChecksPass", state)
	}
}

func TestWatchRequiredChecksClassifiesNoRequiredChecks(t *testing.T) {
	bin, _ := fakeGH(t, `echo "no required checks reported on the 'topic' branch" >&2; exit 1`)
	state, _, err := testClient(bin).WatchRequiredChecks(context.Background(), t.TempDir(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if state != ChecksNone {
		t.Fatalf("state = %v, want ChecksNone", state)
	}
}

func TestWatchRequiredChecksDoesNotTreatFailedCheckTextAsTransient(t *testing.T) {
	for _, output := range []string{
		"integration timeout\tfail\t1m",
		"HTTP 503 coverage\tfail\t1m",
		"connection refused test\tcancel\t1m",
	} {
		t.Run(output, func(t *testing.T) {
			bin, _ := fakeGH(t, "echo '"+output+"' >&2; exit 1")
			state, _, err := testClient(bin).WatchRequiredChecks(context.Background(), t.TempDir(), 42)
			if err != nil {
				t.Fatalf("err = %v, want terminal check failure", err)
			}
			if state != ChecksFail {
				t.Fatalf("state = %v, want ChecksFail", state)
			}
		})
	}
}

// An empty body is not an empty result: gh prints [] when a search found
// nothing, so silence means the call did not really work.
func TestSearchEmptyOutputIsTransient(t *testing.T) {
	bin, _ := fakeGH(t, `exit 0`)
	_, err := testClient(bin).SearchReviewRequested(context.Background(), nil)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
}

func TestSearchEmptyArrayIsAnEmptyResult(t *testing.T) {
	bin, _ := fakeGH(t, `echo "[]"`)
	prs, err := testClient(bin).SearchReviewRequested(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty result should not be an error: %v", err)
	}
	if len(prs) != 0 {
		t.Errorf("got %d pull requests", len(prs))
	}
}

func TestSearchParsesResult(t *testing.T) {
	bin, _ := fakeGH(t, `cat <<'JSON'
[{"author":{"id":"U_1","is_bot":false,"login":"theitger","type":"User"},
  "isDraft":false,"number":2017,
  "repository":{"name":"toaster-api","nameWithOwner":"crumbtray/toaster-api"},
  "title":"Zeiterfassungs-Export","updatedAt":"2026-07-26T21:43:16Z",
  "url":"https://github.com/crumbtray/toaster-api/pull/2017"}]
JSON`)
	prs, err := testClient(bin).SearchReviewRequested(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d pull requests", len(prs))
	}
	p := prs[0]
	if p.Key() != "crumbtray/toaster-api#2017" {
		t.Errorf("Key = %q", p.Key())
	}
	if p.Author.Login != "theitger" || p.Author.IsBot {
		t.Errorf("author = %+v", p.Author)
	}
	if p.UpdatedAt != "2026-07-26T21:43:16Z" || p.IsDraft {
		t.Errorf("pr = %+v", p)
	}
}

const timelinePage = `[
  {"event":"commented","created_at":"2026-07-20T10:00:00Z"},
  {"event":"review_requested","created_at":"2026-07-21T10:00:00Z",
   "requested_reviewer":{"login":"somebody-else"}},
  {"event":"review_requested","created_at":"2026-07-22T10:00:00Z",
   "requested_reviewer":{"login":"yungweng"}},
  {"event":"review_requested","created_at":"2026-07-23T10:00:00Z",
   "requested_team":{"slug":"other-team"}}
]`

const timelinePage2 = `[
  {"event":"review_requested","created_at":"2026-07-24T10:00:00Z",
   "requested_team":{"slug":"bread-council"}},
  {"event":"closed","created_at":"2026-07-25T10:00:00Z"}
]`

func TestLatestReviewRequestPicksNewestForMeOrMyTeam(t *testing.T) {
	bin, _ := fakeGH(t, "cat <<'JSON'\n"+timelinePage+"\nJSON")
	got, err := testClient(bin).LatestReviewRequest(context.Background(),
		"crumbtray/toaster-api", 2017, "yungweng", []string{"bread-council"})
	if err != nil {
		t.Fatal(err)
	}
	// The request for somebody else and the one for a team I am not in do not
	// count, so mine from the 22nd is the newest that does.
	if got != "2026-07-22T10:00:00Z" {
		t.Errorf("got %q", got)
	}
}

// gh merges paginated arrays today but has not always; both shapes must parse.
func TestLatestReviewRequestReadsConcatenatedPages(t *testing.T) {
	bin, _ := fakeGH(t, "cat <<'JSON'\n"+timelinePage+"\n"+timelinePage2+"\nJSON")
	got, err := testClient(bin).LatestReviewRequest(context.Background(),
		"crumbtray/toaster-api", 2017, "yungweng", []string{"bread-council"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026-07-24T10:00:00Z" {
		t.Errorf("got %q, want the team request from the second page", got)
	}
}

func TestLatestReviewRequestNoneIsNotAnError(t *testing.T) {
	bin, _ := fakeGH(t, "cat <<'JSON'\n"+timelinePage+"\nJSON")
	got, err := testClient(bin).LatestReviewRequest(context.Background(),
		"crumbtray/toaster-api", 2017, "nobody", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// A failing timeline call must be distinguishable from "no request found",
// otherwise a network hiccup silently looks like a decision not to review.
func TestLatestReviewRequestReportsFailure(t *testing.T) {
	bin, _ := fakeGH(t, `echo "net/http: TLS handshake timeout" >&2; exit 1`)
	got, err := testClient(bin).LatestReviewRequest(context.Background(),
		"crumbtray/toaster-api", 2014, "yungweng", nil)
	if err == nil {
		t.Fatal("a failed timeline call looked like an empty timeline")
	}
	if !errors.Is(err, ErrTransient) {
		t.Errorf("err = %v, want ErrTransient", err)
	}
	if got != "" {
		t.Errorf("got %q on failure", got)
	}
}

func TestReviewsReadsConcatenatedPages(t *testing.T) {
	bin, _ := fakeGH(t, `printf '%s\n%s\n' \
  '[{"state":"APPROVED","commit_id":"head-1","submitted_at":"2026-07-31T09:00:00Z","user":{"login":"example-user"}}]' \
  '[{"state":"DISMISSED","commit_id":"head-2","user":{"login":"other-user"}}]'`)
	reviews, err := testClient(bin).Reviews(context.Background(), "acme/api", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 2 || reviews[0].CommitID != "head-1" || reviews[0].SubmittedAt.IsZero() || reviews[1].State != "DISMISSED" {
		t.Fatalf("reviews = %+v", reviews)
	}
}

func TestTeamSlugsFor(t *testing.T) {
	teams := []string{"crumbtray/bread-council", "Crumb-GmbH/dev-admins"}
	got := TeamSlugsFor("crumbtray", teams)
	if len(got) != 1 || got[0] != "bread-council" {
		t.Errorf("got %v", got)
	}
	if len(TeamSlugsFor("unrelated", teams)) != 0 {
		t.Error("teams from another organisation leaked in")
	}
}

func TestPRDetailsRejectsEmptyHead(t *testing.T) {
	bin, _ := fakeGH(t, `echo '{"isDraft":false,"state":"OPEN"}'`)
	_, err := testClient(bin).PRDetails(context.Background(), "acme/api", 1)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient for a missing head sha", err)
	}
}

func TestPRDetailsReadsReviewDecision(t *testing.T) {
	bin, _ := fakeGH(t, `
if [[ "$*" != *reviewDecision* || "$*" != *latestReviews* || "$*" != *baseRefName* || "$*" != *mergeStateStatus* || "$*" != *mergeable* ]]; then
  echo 'review policy fields were not requested' >&2
  exit 1
fi
echo '{"baseRefName":"main","headRefOid":"abc123","state":"OPEN","mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","reviewDecision":"CHANGES_REQUESTED","latestReviews":[{"state":"CHANGES_REQUESTED","author":{"login":"example-reviewer"}}]}'`)
	details, err := testClient(bin).PRDetails(context.Background(), "acme/api", 42)
	if err != nil {
		t.Fatal(err)
	}
	if details.ReviewDecision != "CHANGES_REQUESTED" {
		t.Fatalf("review decision = %q", details.ReviewDecision)
	}
	if details.BaseRefName != "main" || len(details.LatestReviews) != 1 ||
		details.LatestReviews[0].Author.Login != "example-reviewer" ||
		details.Mergeable != "CONFLICTING" || details.MergeStateStatus != "DIRTY" {
		t.Fatalf("details = %+v", details)
	}
}

func TestPRStatesReadsABatch(t *testing.T) {
	// One call, one alias per pull request, three different outcomes.
	bin, countFile := fakeGH(t, `echo "$@" > `+t.TempDir()+`/args
cat <<'JSON'
{"data":{
  "p0":{"pullRequest":{"state":"MERGED"}},
  "p1":{"pullRequest":{"state":"OPEN","autoMergeRequest":{"enabledAt":"2026-07-31T08:00:00Z"},"mergeQueueEntry":null}},
  "p2":{"pullRequest":{"state":"CLOSED"}}
}}
JSON`)
	keys := []string{"crumbtray/toaster-api#2035", "crumbtray/bagel-bot#392", "acme/api#7"}
	got, err := testClient(bin).PRStates(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"crumbtray/toaster-api#2035": StateMerged,
		"crumbtray/bagel-bot#392":    StateAutoMerge,
		"acme/api#7":                 StateClosed,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// A dashboard redraws, so this must stay one round trip however many pull
	// requests are on screen.
	if n := calls(t, countFile); n != 1 {
		t.Errorf("made %d calls for %d pull requests, want 1", n, len(keys))
	}
}

func TestPRStatesRecognisesMergeQueueEntry(t *testing.T) {
	bin, _ := fakeGH(t, `echo '{"data":{"p0":{"pullRequest":{"state":"OPEN","autoMergeRequest":null,"mergeQueueEntry":{"position":2}}}}}'`)
	got, err := testClient(bin).PRStates(context.Background(), []string{"acme/api#42"})
	if err != nil {
		t.Fatal(err)
	}
	if got["acme/api#42"] != StateAutoMerge {
		t.Errorf("state = %q, want %q", got["acme/api#42"], StateAutoMerge)
	}
}

// A repository that has gone away answers null. The other pull requests in the
// same batch must still come back.
func TestPRStatesSurvivesAMissingRepository(t *testing.T) {
	bin, _ := fakeGH(t, `cat <<'JSON'
{"data":{"p0":null,"p1":{"pullRequest":{"state":"MERGED"}}},"errors":[{"type":"NOT_FOUND","path":["p0"],"message":"repository not found"}]}
JSON
echo 'gh: repository not found' >&2
exit 1`)
	got, err := testClient(bin).PRStates(context.Background(),
		[]string{"gone/away#1", "acme/api#2"})
	if err != nil {
		t.Fatal(err)
	}
	if got["gone/away#1"] != StateUnavailable {
		t.Errorf("gone/away#1 = %q, want UNAVAILABLE", got["gone/away#1"])
	}
	if got["acme/api#2"] != StateMerged {
		t.Errorf("acme/api#2 = %q, want MERGED", got["acme/api#2"])
	}
}

func TestPRStatesRejectsOtherPartialGraphQLErrors(t *testing.T) {
	bin, _ := fakeGH(t, `cat <<'JSON'
{"data":{"p0":null},"errors":[{"type":"FORBIDDEN","path":["p0"],"message":"permission denied"}]}
JSON
echo 'gh: permission denied' >&2
exit 1`)
	if _, err := testClient(bin).PRStates(context.Background(), []string{"acme/api#42"}); err == nil {
		t.Fatal("a forbidden GraphQL field was reported as success")
	}
}

func TestPRStatesRejectsNotFoundBelowThePullRequestField(t *testing.T) {
	bin, _ := fakeGH(t, `cat <<'JSON'
{"data":{"p0":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["p0","somethingElse"],"message":"field not found"}]}
JSON
echo 'gh: field not found' >&2
exit 1`)
	if _, err := testClient(bin).PRStates(context.Background(), []string{"acme/api#42"}); err == nil {
		t.Fatal("an unexpected nested NOT_FOUND was reported as tolerable")
	}
}

// Repository names reach this from the state file and end up inside a query
// string, so anything that is not a plain owner/name is dropped rather than
// interpolated.
func TestPRStatesDropsUnusableKeys(t *testing.T) {
	bin, countFile := fakeGH(t, `echo '{"data":{}}'`)
	bad := []string{
		`acme/api"){pullRequest(number:1){state}} injected: repository(owner:"x`,
		"no-number",
		"acme/api#notanumber",
		"acme/api#0",
	}
	got, err := testClient(bin).PRStates(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
	// With nothing left to ask about, gh must not be run at all.
	if n := calls(t, countFile); n != 0 {
		t.Errorf("ran gh %d time(s) for keys it could not parse", n)
	}
}

// A pull request number that does not exist in a real repository answers a
// null pullRequest with a NOT_FOUND error whose path descends into the field.
// One such stale key must not freeze the merge states of every other tracked
// pull request; this exact shape once froze the dashboard for good.
func TestPRStatesSurvivesAMissingPullRequest(t *testing.T) {
	bin, _ := fakeGH(t, `cat <<'JSON'
{"data":{"p0":{"pullRequest":null},"p1":{"pullRequest":{"state":"MERGED"}}},"errors":[{"type":"NOT_FOUND","path":["p0","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 2166."}]}
JSON
echo 'gh: Could not resolve to a PullRequest with the number of 2166.' >&2
exit 1`)
	got, err := testClient(bin).PRStates(context.Background(),
		[]string{"acme/api#2166", "acme/api#2170"})
	if err != nil {
		t.Fatal(err)
	}
	if got["acme/api#2166"] != StateUnavailable {
		t.Errorf("acme/api#2166 = %q, want UNAVAILABLE", got["acme/api#2166"])
	}
	if got["acme/api#2170"] != StateMerged {
		t.Errorf("acme/api#2170 = %q, want MERGED", got["acme/api#2170"])
	}
}

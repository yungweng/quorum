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
  "repository":{"name":"project-phoenix","nameWithOwner":"moto-nrw/project-phoenix"},
  "title":"Zeiterfassungs-Export","updatedAt":"2026-07-26T21:43:16Z",
  "url":"https://github.com/moto-nrw/project-phoenix/pull/2017"}]
JSON`)
	prs, err := testClient(bin).SearchReviewRequested(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d pull requests", len(prs))
	}
	p := prs[0]
	if p.Key() != "moto-nrw/project-phoenix#2017" {
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
   "requested_team":{"slug":"moto-vorstand"}},
  {"event":"closed","created_at":"2026-07-25T10:00:00Z"}
]`

func TestLatestReviewRequestPicksNewestForMeOrMyTeam(t *testing.T) {
	bin, _ := fakeGH(t, "cat <<'JSON'\n"+timelinePage+"\nJSON")
	got, err := testClient(bin).LatestReviewRequest(context.Background(),
		"moto-nrw/project-phoenix", 2017, "yungweng", []string{"moto-vorstand"})
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
		"moto-nrw/project-phoenix", 2017, "yungweng", []string{"moto-vorstand"})
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
		"moto-nrw/project-phoenix", 2017, "nobody", nil)
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
		"moto-nrw/project-phoenix", 2014, "yungweng", nil)
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

func TestTeamSlugsFor(t *testing.T) {
	teams := []string{"moto-nrw/moto-vorstand", "Inference-GmbH/dev-admins"}
	got := TeamSlugsFor("moto-nrw", teams)
	if len(got) != 1 || got[0] != "moto-vorstand" {
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

package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
)

type cancelAwareReviewer struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (f *cancelAwareReviewer) Run(ctx context.Context, _ review.Options) (*review.Result, error) {
	close(f.started)
	select {
	case <-ctx.Done():
		close(f.canceled)
		return nil, ctx.Err()
	case <-f.release:
		return &review.Result{}, nil
	}
}

func TestDiscardReviewCancelsRunningReview(t *testing.T) {
	reviewer := &cancelAwareReviewer{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
	r := &run{
		p:   &Pipeline{Review: reviewer},
		ctx: context.Background(),
		rep: NopReporter{},
	}
	r.startReview(1)
	<-reviewer.started

	done := make(chan struct{})
	go func() {
		r.discardReview()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		close(reviewer.release)
		<-done
		t.Fatal("discardReview waited for the review instead of cancelling it")
	}
	select {
	case <-reviewer.canceled:
	default:
		t.Fatal("discardReview returned without cancelling the review context")
	}
	if r.review != nil {
		t.Fatal("discardReview kept the cancelled review")
	}
}

func TestCIWaitPublishesItsPhase(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fastCIWait(t, time.Hour)
	bin, _ := ciFixture{
		prChecks: "test\tpass",
		branch:   []string{noProtection},
		rules:    []string{noRules},
		checks:   []string{checkRunsJSON("success")},
		statuses: []string{`{"statuses":[]}`},
		runs:     []string{runsJSON("completed", "success")},
	}.install(t)
	before := time.Now()
	r := &run{
		p:       &Pipeline{GH: gh.New(bin)},
		o:       Options{Repo: "acme/api", RepoRoot: root},
		ctx:     context.Background(),
		rep:     NopReporter{},
		pr:      gh.FullPR{Number: 7, BaseRefName: "main"},
		headSHA: "abc123",
		root:    root,
		logDir:  logDir,
		prog: Progress{
			Phase:      PhaseFix,
			Since:      before.Add(-time.Hour),
			CI:         CIRed,
			SideReview: SideReviewPosted,
		},
	}

	if err := r.ensureCIGreen(); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadProgress(root)
	if !ok {
		t.Fatal("CI progress was not published")
	}
	if got.Phase != PhaseCI || got.Since.Before(before) {
		t.Errorf("phase = %q since %v, want %q starting after %v", got.Phase, got.Since, PhaseCI, before)
	}
	if got.CI != CIGreen {
		t.Errorf("CI = %q, want %q", got.CI, CIGreen)
	}
	if got.SideReview != "" {
		t.Errorf("side review %q outlived the CI wait", got.SideReview)
	}
}

func TestWatchCIRetriesTransientWatchFailure(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "gh")
	api, _ := ciFixture{
		branch:   []string{noProtection},
		rules:    []string{noRules},
		checks:   []string{checkRunsJSON("success")},
		statuses: []string{`{"statuses":[]}`},
		runs:     []string{runsJSON("completed", "success")},
	}.install(t)
	// Only the watch is counted; the confirmation's API reads go to the
	// fixture.
	script := `#!/bin/sh
if test "$1" = api; then exec '` + api + `' "$@"; fi
n=0
test ! -f "` + countFile + `" || n=$(cat "` + countFile + `")
n=$((n + 1))
printf '%s' "$n" > "` + countFile + `"
if test "$n" -eq 1; then
  echo 'net/http: TLS handshake timeout' >&2
  exit 1
fi
echo 'checks passed'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p: &Pipeline{GH: gh.New(bin)}, o: Options{Repo: "acme/api", RepoRoot: dir},
		ctx: context.Background(), logDir: dir, pr: gh.FullPR{Number: 42, BaseRefName: "main"},
		headSHA: "abc123",
	}

	state, err := r.watchCI()
	if err != nil {
		t.Fatal(err)
	}
	if state != gh.ChecksPass {
		t.Fatalf("state = %v, want ChecksPass", state)
	}
	if calls, err := os.ReadFile(countFile); err != nil || string(calls) != "2" {
		t.Fatalf("watch calls = %q, err = %v", calls, err)
	}
}

type postingReviewer struct{ release chan struct{} }

func (f *postingReviewer) Run(context.Context, review.Options) (*review.Result, error) {
	<-f.release
	return &review.Result{Posted: true}, nil
}

// The first review runs beside the CI wait and posts its comment on its own. A
// wait that does not say so reads as a run that posted and then forgot about it.
func TestCIWaitReportsTheReviewBesideIt(t *testing.T) {
	root := t.TempDir()
	reviewer := &postingReviewer{release: make(chan struct{})}
	r := &run{
		p:    &Pipeline{Review: reviewer},
		ctx:  context.Background(),
		rep:  NopReporter{},
		root: root,
	}
	if got := r.sideReview(); got != "" {
		t.Fatalf("a wait without a review reported %q", got)
	}

	r.startReview(1)
	if got := r.sideReview(); got != "review round 1 running" {
		t.Errorf("running review reported as %q", got)
	}
	if got, _ := ReadProgress(root); got.SideReview != SideReviewRunning {
		t.Errorf("published side review = %q, want %q", got.SideReview, SideReviewRunning)
	}

	close(reviewer.release)
	out := <-r.review.done
	r.review.done <- out
	if got := r.sideReview(); got != "review round 1 posted" {
		t.Errorf("posted review reported as %q", got)
	}
	if got, _ := ReadProgress(root); got.SideReview != SideReviewPosted {
		t.Errorf("published side review = %q, want %q", got.SideReview, SideReviewPosted)
	}
}

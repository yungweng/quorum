package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/gh"
)

// ciFixture stands in for GitHub during a CI wait. `gh pr checks` answers
// with prChecks (exit 0: everything it knows passed), and every API endpoint
// the confirmation reads answers from a sequence: the n-th call gets the n-th
// response, and calls past the end repeat the last one.
type ciFixture struct {
	prChecks string
	branch   []string
	rules    []string
	checks   []string
	statuses []string
	runs     []string
	flows    []string
}

const (
	noProtection = `{"protection":{"required_status_checks":{"enforcement_level":"off","contexts":[]}}}`
	ciGateRule   = `{"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["ci-gate"]}}}`
	noRules      = `[]`
	externalOnly = `{"statuses":[{"context":"secret-scan","state":"success"}]}`
	noCheckRuns  = `{"check_runs":[]}`
	noRuns       = `{"workflow_runs":[]}`
	someWorkflow = `{"workflows":[{"path":".github/workflows/ci.yml","state":"active"}]}`
)

func checkRunsJSON(gate string) string {
	switch gate {
	case "":
		return noCheckRuns
	case "pending":
		return `{"check_runs":[{"id":1,"name":"ci-gate","status":"in_progress","conclusion":null}]}`
	default:
		return fmt.Sprintf(`{"check_runs":[{"id":1,"name":"ci-gate","status":"completed","conclusion":%q}]}`, gate)
	}
}

func runsJSON(status, conclusion string) string {
	return fmt.Sprintf(`{"workflow_runs":[{"id":11,"name":"main","workflow_id":3,"event":"pull_request","status":%q,"conclusion":%q,"html_url":"https://github.com/acme/api/actions/runs/11"}]}`,
		status, conclusion)
}

// install writes the fake gh and its responses, and returns the binary and a
// function that reports how often an endpoint was asked.
func (f ciFixture) install(t *testing.T) (string, func(endpoint string) int) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for endpoint, seq := range map[string][]string{
		"branch": f.branch, "rules": f.rules, "check-runs": f.checks,
		"status": f.statuses, "runs": f.runs, "workflows": f.flows,
	} {
		for i, body := range seq {
			write(fmt.Sprintf("%s.%d", endpoint, i+1), body)
		}
		if len(seq) > 0 {
			write(endpoint+".last", seq[len(seq)-1])
		}
	}
	write("pr-checks", f.prChecks)
	bin := filepath.Join(dir, "gh")
	script := `#!/bin/sh
dir='` + dir + `'
if test "$1" = pr && test "$2" = checks; then
  n=$(cat "$dir/pr-checks.count" 2>/dev/null || echo 0); echo $((n + 1)) > "$dir/pr-checks.count"
  cat "$dir/pr-checks"; exit 0
fi
if test "$1" != api; then echo "unexpected: $*" >&2; exit 1; fi
ep=
for a in "$@"; do
  case "$a" in
    *actions/runs*) ep=runs ;;
    *actions/workflows*) ep=workflows ;;
    *check-runs*) ep=check-runs ;;
    */status\?*) ep=status ;;
    *rules/branches/*) ep=rules ;;
    *branches/*) ep=branch ;;
  esac
done
test -n "$ep" || { echo "unexpected api call: $*" >&2; exit 1; }
n=$(cat "$dir/$ep.count" 2>/dev/null || echo 0); n=$((n + 1)); echo $n > "$dir/$ep.count"
if test -f "$dir/$ep.$n"; then cat "$dir/$ep.$n"; elif test -f "$dir/$ep.last"; then cat "$dir/$ep.last"; else echo "no fixture for $ep" >&2; exit 1; fi
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func(endpoint string) int {
		data, _ := os.ReadFile(filepath.Join(dir, endpoint+".count"))
		n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		return n
	}
}

// fastCIWait shortens the CI wait's polls and its registration window for one
// test.
func fastCIWait(t *testing.T, window time.Duration) {
	t.Helper()
	pending, none, timeout := ciPendingPoll, ciNoChecksPoll, noChecksTimeout
	ciPendingPoll, ciNoChecksPoll, noChecksTimeout = time.Millisecond, time.Millisecond, window
	t.Cleanup(func() { ciPendingPoll, ciNoChecksPoll, noChecksTimeout = pending, none, timeout })
}

func ciTestRun(t *testing.T, bin string) *run {
	t.Helper()
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := gh.New(bin)
	c.Backoff = time.Millisecond
	return &run{
		p:       &Pipeline{GH: c},
		o:       Options{Repo: "acme/api", RepoRoot: root},
		ctx:     context.Background(),
		rep:     NopReporter{},
		pr:      gh.FullPR{Number: 42, BaseRefName: "main"},
		headSHA: "abc123",
		root:    root,
		logDir:  logDir,
	}
}

// Right after a push GitHub may know only an external app's status. `gh pr
// checks` then passes, but the Actions run of the head registers a moment
// later and must be waited for before CI counts as green.
func TestCIWaitWaitsForAnActionsRunThatRegistersLate(t *testing.T) {
	fastCIWait(t, time.Hour)
	bin, asked := ciFixture{
		prChecks: "secret-scan\tpass",
		branch:   []string{noProtection},
		rules:    []string{noRules},
		checks:   []string{noCheckRuns},
		statuses: []string{externalOnly},
		runs: []string{
			noRuns, noRuns,
			runsJSON("queued", ""),
			runsJSON("in_progress", ""),
			runsJSON("completed", "success"),
		},
		flows: []string{someWorkflow},
	}.install(t)
	r := ciTestRun(t, bin)

	state, err := r.watchCI()
	if err != nil {
		t.Fatal(err)
	}
	if state != gh.ChecksPass {
		t.Fatalf("state = %v, want ChecksPass", state)
	}
	if n := asked("runs"); n != 5 {
		t.Fatalf("workflow runs read %d times, want 5: green before the late run finished", n)
	}
}

// The required check is the merge gate. While it is still running - or not
// reported yet - CI is not green, whatever else already passed.
func TestCIWaitWaitsForARunningRequiredCheck(t *testing.T) {
	fastCIWait(t, time.Hour)
	bin, asked := ciFixture{
		prChecks: "secret-scan\tpass",
		branch:   []string{ciGateRule},
		rules:    []string{noRules},
		checks:   []string{checkRunsJSON(""), checkRunsJSON("pending"), checkRunsJSON("pending"), checkRunsJSON("success")},
		statuses: []string{externalOnly},
		runs:     []string{runsJSON("completed", "success")},
		flows:    []string{someWorkflow},
	}.install(t)
	r := ciTestRun(t, bin)

	state, err := r.watchCI()
	if err != nil {
		t.Fatal(err)
	}
	if state != gh.ChecksPass {
		t.Fatalf("state = %v, want ChecksPass", state)
	}
	if n := asked("check-runs"); n != 4 {
		t.Fatalf("checks read %d times, want 4: green before the required check passed", n)
	}
	if n := asked("branch"); n != 1 {
		t.Fatalf("branch rules read %d times, want once per wait", n)
	}
}

// A required check that never reports keeps the pull request blocked. After
// the registration window the run stops - neither green nor a CI fix session.
func TestCIWaitStopsOnARequiredCheckThatNeverReports(t *testing.T) {
	fastCIWait(t, 50*time.Millisecond)
	bin, _ := ciFixture{
		prChecks: "secret-scan\tpass",
		branch:   []string{ciGateRule},
		rules:    []string{noRules},
		checks:   []string{noCheckRuns},
		statuses: []string{externalOnly},
		runs:     []string{noRuns},
		flows:    []string{someWorkflow},
	}.install(t)
	r := ciTestRun(t, bin)

	err := r.ensureCIGreen()
	if !errors.Is(err, ErrCIUnconfirmed) || !strings.Contains(err.Error(), "ci-gate") {
		t.Fatalf("err = %v, want ErrCIUnconfirmed naming ci-gate", err)
	}
	if r.prog.CI == CIGreen {
		t.Fatal("an unreported required check was published as green")
	}
}

// A workflow run that fails before any job starts (a rejected workflow file,
// an Actions outage) reports no check at all. It must not read as green, and
// it is not a code failure a fix session could repair.
func TestCIWaitStopsOnAWorkflowRunThatFailedWithoutJobs(t *testing.T) {
	// GitHub reports a rejected workflow file as "failure" and a run it could
	// not start as "startup_failure"; neither leaves a check behind.
	for _, conclusion := range []string{"failure", "startup_failure"} {
		t.Run(conclusion, func(t *testing.T) {
			fastCIWait(t, time.Hour)
			bin, _ := ciFixture{
				prChecks: "secret-scan\tpass",
				branch:   []string{noProtection},
				rules:    []string{noRules},
				checks:   []string{noCheckRuns},
				statuses: []string{externalOnly},
				runs:     []string{runsJSON("completed", conclusion)},
				flows:    []string{someWorkflow},
			}.install(t)
			r := ciTestRun(t, bin)

			err := r.ensureCIGreen()
			if !errors.Is(err, ErrCIUnconfirmed) || !strings.Contains(err.Error(), conclusion) {
				t.Fatalf("err = %v, want ErrCIUnconfirmed naming the failed run", err)
			}
			if r.prog.CI == CIGreen || r.ciFixTotal != 0 {
				t.Fatalf("CI = %q after %d fix sessions, want neither green nor a fix session", r.prog.CI, r.ciFixTotal)
			}
		})
	}
}

// A failed check bound to the head goes to the CI fix path, as a failure
// `gh pr checks` reports would.
func TestCIConfirmationReportsAFailedCheckOfTheHead(t *testing.T) {
	fastCIWait(t, time.Hour)
	bin, _ := ciFixture{
		prChecks: "secret-scan\tpass",
		branch:   []string{ciGateRule},
		rules:    []string{noRules},
		checks:   []string{checkRunsJSON("failure")},
		statuses: []string{externalOnly},
		runs:     []string{runsJSON("completed", "failure")},
		flows:    []string{someWorkflow},
	}.install(t)
	r := ciTestRun(t, bin)

	state, err := r.watchCI()
	if err != nil {
		t.Fatal(err)
	}
	if state != gh.ChecksFail {
		t.Fatalf("state = %v, want ChecksFail", state)
	}
}

// A repository without workflow files never gets an Actions run, so an
// external check alone is a complete result and needs no registration window.
func TestCIWaitAcceptsExternalChecksWhenNoWorkflowExists(t *testing.T) {
	fastCIWait(t, time.Hour)
	bin, asked := ciFixture{
		prChecks: "secret-scan\tpass",
		branch:   []string{noProtection},
		rules:    []string{noRules},
		checks:   []string{noCheckRuns},
		statuses: []string{externalOnly},
		runs:     []string{noRuns},
		flows:    []string{`{"workflows":[]}`},
	}.install(t)
	r := ciTestRun(t, bin)

	done := make(chan struct{})
	var state gh.CheckState
	var err error
	go func() {
		state, err = r.watchCI()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait sat out the registration window although no workflow exists")
	}
	if err != nil {
		t.Fatal(err)
	}
	if state != gh.ChecksPass {
		t.Fatalf("state = %v, want ChecksPass", state)
	}
	if n := asked("workflows"); n != 1 {
		t.Fatalf("workflows read %d times, want 1", n)
	}
}

// Nothing reported and nothing required: after the window this is a PR
// without CI, as before.
func TestCIWaitReportsNoChecksAfterTheWindow(t *testing.T) {
	fastCIWait(t, 50*time.Millisecond)
	bin, _ := ciFixture{
		prChecks: "",
		branch:   []string{noProtection},
		rules:    []string{noRules},
		checks:   []string{noCheckRuns},
		statuses: []string{`{"statuses":[]}`},
		runs:     []string{noRuns},
		flows:    []string{someWorkflow},
	}.install(t)
	r := ciTestRun(t, bin)

	state, err := r.watchCI()
	if err != nil {
		t.Fatal(err)
	}
	if state != gh.ChecksNone {
		t.Fatalf("state = %v, want ChecksNone", state)
	}
}

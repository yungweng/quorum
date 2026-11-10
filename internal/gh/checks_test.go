package gh

import (
	"context"
	"reflect"
	"testing"
)

func TestRequiredChecksMergesBranchProtectionAndRulesets(t *testing.T) {
	bin, _ := fakeGH(t, `
case "$*" in
  "api repos/acme/api/branches/main")
    echo '{"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["ci-gate"],"checks":[{"context":"ci-gate","app_id":1},{"context":"lint"}]}}}' ;;
  "api --paginate repos/acme/api/rules/branches/main?per_page=100")
    echo '[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build"},{"context":"lint"}]}}]' ;;
  *) echo "unexpected arguments: $*" >&2; exit 1 ;;
esac`)
	got, err := testClient(bin).RequiredChecks(context.Background(), "acme/api", "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"build", "ci-gate", "lint"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
}

func TestRequiredChecksIgnoresDisabledProtection(t *testing.T) {
	bin, _ := fakeGH(t, `
case "$*" in
  *rules/branches*) echo '[]' ;;
  *) echo '{"protection":{"required_status_checks":{"enforcement_level":"off","contexts":["stale"]}}}' ;;
esac`)
	got, err := testClient(bin).RequiredChecks(context.Background(), "acme/api", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("required = %v, want none", got)
	}
}

func TestWorkflowRunsKeepsTheNewestRunPerWorkflowAndEvent(t *testing.T) {
	bin, _ := fakeGH(t, `
if [[ "$*" != "api --paginate repos/acme/api/actions/runs?head_sha=abc123&per_page=100" ]]; then
  echo "unexpected arguments: $*" >&2; exit 1
fi
echo '{"workflow_runs":[{"id":1,"name":"PR body","workflow_id":7,"event":"pull_request","status":"completed","conclusion":"cancelled"}]}'
echo '{"workflow_runs":[{"id":2,"name":"PR body","workflow_id":7,"event":"pull_request","status":"completed","conclusion":"success"},{"id":3,"name":"main","workflow_id":8,"event":"pull_request","status":"in_progress","conclusion":null},{"id":4,"name":"main","workflow_id":8,"event":"push","status":"completed","conclusion":"startup_failure"}]}'`)
	runs, err := testClient(bin).WorkflowRuns(context.Background(), "acme/api", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, run := range runs {
		got = append(got, run.Name+"/"+run.Event+"="+run.Bucket())
	}
	want := []string{"PR body/pull_request=pass", "main/pull_request=pending", "main/push=fail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
}

func TestCommitChecksBindsToTheShaAndKeepsTheNewestRun(t *testing.T) {
	bin, _ := fakeGH(t, `
case "$*" in
  "api --paginate repos/acme/api/commits/abc123/check-runs?per_page=100&filter=latest")
    echo '{"check_runs":[{"id":5,"name":"test","status":"completed","conclusion":"failure"},{"id":9,"name":"test","status":"completed","conclusion":"success"},{"id":6,"name":"ci-gate","status":"queued","conclusion":null},{"id":7,"name":"docs","status":"completed","conclusion":"skipped"}]}' ;;
  "api repos/acme/api/commits/abc123/status?per_page=100")
    echo '{"statuses":[{"context":"GitGuardian","state":"success"},{"context":"deploy","state":"error"}]}' ;;
  *) echo "unexpected arguments: $*" >&2; exit 1 ;;
esac`)
	checks, err := testClient(bin).CommitChecks(context.Background(), "acme/api", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	want := []CommitCheck{
		{"GitGuardian", BucketPass}, {"ci-gate", BucketPending}, {"deploy", BucketFail},
		{"docs", BucketPass}, {"test", BucketPass},
	}
	if !reflect.DeepEqual(checks, want) {
		t.Fatalf("checks = %v, want %v", checks, want)
	}
}

func TestHasActiveWorkflowsIgnoresDynamicAndDisabledWorkflows(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"none":     {`{"workflows":[]}`, false},
		"dynamic":  {`{"workflows":[{"path":"dynamic/github-code-scanning/codeql","state":"active"}]}`, false},
		"disabled": {`{"workflows":[{"path":".github/workflows/ci.yml","state":"disabled_manually"}]}`, false},
		"active":   {`{"workflows":[{"path":".github/workflows/ci.yml","state":"active"}]}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			bin, _ := fakeGH(t, "echo '"+tc.body+"'")
			got, err := testClient(bin).HasActiveWorkflows(context.Background(), "acme/api")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("active = %v, want %v", got, tc.want)
			}
		})
	}
}

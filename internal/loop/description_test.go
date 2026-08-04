package loop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/target"
)

func descriptionTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

type descriptionPaths struct {
	stdin    string
	editArgs string
	ghBin    string
	original string
}

func descriptionFixture(t *testing.T, original, final string) (*run, *Result, descriptionPaths) {
	t.Helper()
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	for _, dir := range []string{worktree, filepath.Join(root, "messages"), filepath.Join(root, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	descriptionTestGit(t, worktree, "init", "-q")
	descriptionTestGit(t, worktree, "config", "user.email", "example@example.invalid")
	descriptionTestGit(t, worktree, "config", "user.name", "Example User")
	descriptionTestGit(t, worktree, "config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(worktree, "retry.go"), []byte("package retry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	descriptionTestGit(t, worktree, "add", "retry.go")
	descriptionTestGit(t, worktree, "commit", "-q", "-m", "Initial fixture")
	head := descriptionTestGit(t, worktree, "rev-parse", "HEAD")

	resultPath := filepath.Join(root, "generated.md")
	if err := os.WriteFile(resultPath, []byte(final), 0o644); err != nil {
		t.Fatal(err)
	}
	stdinPath := filepath.Join(root, "description-stdin")
	codexBin := filepath.Join(root, "codex")
	codexScript := `#!/bin/sh
set -eu
case " $* " in
  *" --sandbox read-only "*) ;;
  *) echo "read-only sandbox missing" >&2; exit 9 ;;
esac
case " $* " in
  *" --dangerously-bypass-approvals-and-sandbox "*) echo "sandbox bypass present" >&2; exit 10 ;;
esac
cat > "` + stdinPath + `"
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift 2; continue; fi
  shift
done
cp "` + resultPath + `" "$out"
`
	if err := os.WriteFile(codexBin, []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}

	originalPR := gh.FullPR{
		Number: 42, Title: "Bound retries", Body: original, State: "OPEN",
		HeadRefName: "feature/retries", HeadRefOid: head, BaseRefName: "main",
	}
	originalJSON, _ := json.Marshal(originalPR)
	originalJSONPath := filepath.Join(root, "original.json")
	if err := os.WriteFile(originalJSONPath, originalJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(root, "gh-count")
	editArgsPath := filepath.Join(root, "gh-edit-args")
	ghBin := filepath.Join(root, "gh")
	ghScript := `#!/bin/sh
set -eu
n=$(cat "` + countPath + `" 2>/dev/null || echo 0)
n=$((n+1)); echo "$n" > "` + countPath + `"
case " $* " in
  *" edit "*) echo "$*" > "` + editArgsPath + `"; exit 0 ;;
esac
case "$n" in
  1|2) cat "` + originalJSONPath + `" ;;
  *) echo "unexpected gh call $n: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(ghBin, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	client := gh.New(ghBin)
	client.Attempts = 1
	client.Timeout = 10 * time.Second
	g := git.New("git")
	r := &run{
		p: &Pipeline{GH: client, Git: g},
		o: Options{
			Repo: "acme/api", RepoRoot: worktree, Post: true,
			ReviewModel: "test-model", ReviewEffort: "low", CodexBin: codexBin,
		},
		ctx: context.Background(), rep: NopReporter{},
		target: target.Target{PR: originalPR}, pr: originalPR,
		worktree: worktree, msgDir: filepath.Join(root, "messages"), logDir: filepath.Join(root, "logs"),
		env: envexec.Env{Worktree: worktree},
	}
	paths := descriptionPaths{
		stdin: stdinPath, editArgs: editArgsPath, ghBin: ghBin, original: originalJSONPath,
	}
	return r, &Result{PR: originalPR, Converged: true}, paths
}

func TestFinishPRDescriptionUpdatesChangedBody(t *testing.T) {
	original := "## Summary\n\nBound retry attempts.\n"
	final := "## Summary\n\nBounds retries by the caller deadline.\n"
	r, result, paths := descriptionFixture(t, original, final)
	if err := r.finishPRDescription(result); err != nil {
		t.Fatal(err)
	}
	if result.PRDescriptionCurrent || !result.PRDescriptionUpdated || result.PRDescriptionFile == "" {
		t.Fatalf("result = %+v", result)
	}
	stdin, err := os.ReadFile(paths.stdin)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != original {
		t.Fatalf("generator stdin = %q, want original body", stdin)
	}
	generated, err := os.ReadFile(result.PRDescriptionFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(generated) != strings.TrimSpace(final)+"\n" {
		t.Fatalf("generated body = %q", generated)
	}
	editArgs, err := os.ReadFile(paths.editArgs)
	if err != nil {
		t.Fatalf("gh received no edit call: %v", err)
	}
	want := "pr edit 42 --body-file " + result.PRDescriptionFile
	if strings.TrimSpace(string(editArgs)) != want {
		t.Fatalf("gh edit args = %q, want %q", editArgs, want)
	}
}

func TestFinishPRDescriptionRecognizesCurrentBody(t *testing.T) {
	body := "## Summary\n\nBound retry attempts.\n"
	r, result, paths := descriptionFixture(t, body, body)
	if err := r.finishPRDescription(result); err != nil {
		t.Fatal(err)
	}
	if !result.PRDescriptionCurrent || result.PRDescriptionUpdated {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(paths.editArgs); err == nil {
		t.Fatal("gh received an edit call for an unchanged body")
	}
}

func TestFinishPRDescriptionKeepsCandidateWhenUpdateFails(t *testing.T) {
	original := "## Summary\n\nBound retry attempts.\n"
	final := "## Summary\n\nBounds retries by the caller deadline.\n"
	r, result, paths := descriptionFixture(t, original, final)
	ghScript := `#!/bin/sh
set -eu
case " $* " in
  *" edit "*) echo "update rejected" >&2; exit 1 ;;
esac
cat "` + paths.original + `"
`
	if err := os.WriteFile(paths.ghBin, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	err := r.finishPRDescription(result)
	if err == nil || !strings.Contains(err.Error(), "updating PR description") {
		t.Fatalf("error = %v, want update failure", err)
	}
	if result.PRDescriptionUpdated {
		t.Fatalf("result = %+v", result)
	}
	if result.PRDescriptionFile == "" {
		t.Fatal("candidate path was not recorded")
	}
	if _, statErr := os.Stat(result.PRDescriptionFile); statErr != nil {
		t.Fatalf("candidate file missing: %v", statErr)
	}
}

func TestFinishPRDescriptionSkipsWhenPostingIsDisabled(t *testing.T) {
	r := &run{o: Options{Post: false}, target: target.Target{}}
	if err := r.finishPRDescription(&Result{}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPRDescriptionTargetRefusesConcurrentChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*gh.FullPR)
		want   string
	}{
		"body edit": {
			mutate: func(pr *gh.FullPR) { pr.Body = "A human clarified the rollout.\n" },
			want:   "rejecting the stale generated candidate",
		},
		"head move": {
			mutate: func(pr *gh.FullPR) { pr.HeadRefOid = strings.Repeat("b", 40) },
			want:   "PR head moved",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			original := gh.FullPR{
				Number: 42, Body: "Original body.\n", State: "OPEN",
				HeadRefOid: strings.Repeat("a", 40),
			}
			latest := original
			tc.mutate(&latest)
			payload, err := json.Marshal(latest)
			if err != nil {
				t.Fatal(err)
			}
			payloadPath := filepath.Join(root, "pr.json")
			if err := os.WriteFile(payloadPath, payload, 0o644); err != nil {
				t.Fatal(err)
			}
			ghBin := filepath.Join(root, "gh")
			script := "#!/bin/sh\ncat \"" + payloadPath + "\"\n"
			if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			client := gh.New(ghBin)
			client.Attempts = 1
			r := &run{
				p: &Pipeline{GH: client}, o: Options{RepoRoot: root},
				ctx: context.Background(), pr: original,
			}
			err = r.checkPRDescriptionTarget(original.HeadRefOid)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestReadFinalPRDescriptionRejectsUnsafeShape(t *testing.T) {
	for name, body := range map[string]string{
		"empty":  " \n",
		"fenced": "```markdown\nbody\n```\n",
		"large":  strings.Repeat("x", maxPRDescriptionBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "body.md")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := readFinalPRDescription(path); err == nil {
				t.Fatal("unsafe description was accepted")
			}
		})
	}
}

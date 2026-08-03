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

func descriptionFixture(t *testing.T, original, final string) (*run, *Result, string, string) {
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
	finalPR := originalPR
	finalPR.Body = strings.TrimSpace(final) + "\n"
	originalJSON, _ := json.Marshal(originalPR)
	finalJSON, _ := json.Marshal(finalPR)
	originalJSONPath := filepath.Join(root, "original.json")
	finalJSONPath := filepath.Join(root, "final.json")
	if err := os.WriteFile(originalJSONPath, originalJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalJSONPath, finalJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(root, "gh-count")
	editedPath := filepath.Join(root, "edited-body")
	ghBin := filepath.Join(root, "gh")
	ghScript := `#!/bin/sh
set -eu
n=$(cat "` + countPath + `" 2>/dev/null || echo 0)
n=$((n+1)); echo "$n" > "` + countPath + `"
case "$n" in
  1|2) cat "` + originalJSONPath + `" ;;
  3) test "$1 $2 $3 $4" = "pr edit 42 --body-file"; cp "$5" "` + editedPath + `" ;;
  4) cat "` + finalJSONPath + `" ;;
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
	return r, &Result{PR: originalPR, Converged: true}, stdinPath, editedPath
}

func TestFinishPRDescriptionUpdatesExactlyOnce(t *testing.T) {
	original := "## Summary\n\nBound retry attempts.\n"
	final := "## Summary\n\nBounds retries by the caller deadline.\n"
	r, result, stdinPath, editedPath := descriptionFixture(t, original, final)
	if err := r.finishPRDescription(result); err != nil {
		t.Fatal(err)
	}
	if !result.PRDescriptionUpdated || result.PRDescriptionFile == "" {
		t.Fatalf("result = %+v", result)
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != original {
		t.Fatalf("generator stdin = %q, want original body", stdin)
	}
	edited, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(edited) != strings.TrimSpace(final)+"\n" {
		t.Fatalf("edited body = %q", edited)
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
			want:   "refusing to overwrite",
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

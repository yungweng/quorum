package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentHookChecksOnlyRelevantSessionChanges(t *testing.T) {
	t.Setenv("GO111MODULE", "off")
	repo := newGitRepository(t)
	writeTestFile(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	writeTestFile(t, repo, "Makefile", ".PHONY: check\ncheck:\n\t@test -z \"$${GO111MODULE:-}\"\n\t@printf x >> .check-ran\n\t@test ! -f .fail\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")

	runAgentHook(t, repo, "session-1", "start", false, 0)
	runAgentHook(t, repo, "session-1", "stop", false, 0)
	assertFileMissing(t, filepath.Join(repo, ".check-ran"))

	writeTestFile(t, repo, "README.md", "documentation only\n")
	runAgentHook(t, repo, "session-1", "stop", false, 0)
	assertFileMissing(t, filepath.Join(repo, ".check-ran"))

	writeTestFile(t, repo, "main.go", "package main\n\nfunc main() { println(\"first\") }\n")
	writeTestFile(t, repo, ".fail", "fail the fake check\n")
	stdout, stderr := runAgentCommandHook(t, repo, "session-1", "stop", false, 0)
	var decision struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stdout), &decision); err != nil {
		t.Fatalf("decode blocking decision %q: %v", stdout, err)
	}
	if decision.Decision != "block" || !strings.Contains(decision.Reason, "Quality checks failed") {
		t.Fatalf("blocking decision = %#v", decision)
	}
	if stderr != "" {
		t.Fatalf("blocking command stderr = %q", stderr)
	}
	assertFileContents(t, filepath.Join(repo, ".check-ran"), "x")

	stderr = runAgentHook(t, repo, "session-1", "stop", true, 0)
	if !strings.Contains(stderr, "avoid a hook loop") {
		t.Fatalf("loop feedback = %q", stderr)
	}
	assertFileContents(t, filepath.Join(repo, ".check-ran"), "x")

	if err := os.Remove(filepath.Join(repo, ".fail")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repo, "main.go", "package main\n\nfunc main() { println(\"fixed\") }\n")
	runAgentHook(t, repo, "session-1", "stop", true, 0)
	assertFileContents(t, filepath.Join(repo, ".check-ran"), "xx")

	runAgentHook(t, repo, "session-1", "stop", false, 0)
	assertFileContents(t, filepath.Join(repo, ".check-ran"), "xx")

	runAgentHook(t, repo, "session-1", "end", false, 0)
	stateDir := strings.TrimSpace(git(t, repo, "rev-parse", "--path-format=absolute", "--git-path", "quorum-hook-state"))
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("state directory still contains %d entries", len(entries))
	}
}

func TestAgentHookChecksGoWorkspaceChanges(t *testing.T) {
	repo := newGitRepository(t)
	writeTestFile(t, repo, "Makefile", ".PHONY: check\ncheck:\n\t@printf x >> .check-ran\n")
	git(t, repo, "add", "Makefile")
	git(t, repo, "commit", "-m", "initial")

	runAgentHook(t, repo, "workspace-session", "start", false, 0)
	writeTestFile(t, repo, "go.work", "go 1.26\n")
	runAgentHook(t, repo, "workspace-session", "stop", false, 0)
	assertFileContents(t, filepath.Join(repo, ".check-ran"), "x")

	writeTestFile(t, repo, "go.work.sum", "example checksum\n")
	runAgentHook(t, repo, "workspace-session", "stop", false, 0)
	assertFileContents(t, filepath.Join(repo, ".check-ran"), "xx")
}

func TestHookCommandsExitOutsideRepository(t *testing.T) {
	type commandHook struct {
		Command string `json:"command"`
	}
	type matcher struct {
		Hooks []commandHook `json:"hooks"`
	}
	var config struct {
		Hooks map[string][]matcher `json:"hooks"`
	}

	for _, name := range []string{".claude/settings.json", ".codex/hooks.json"} {
		contents, err := os.ReadFile(filepath.Join(projectRoot(t), filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(contents, &config); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		for event, matchers := range config.Hooks {
			for _, matcher := range matchers {
				for _, hook := range matcher.Hooks {
					if event == "SessionEnd" && !strings.Contains(hook.Command, "/scripts/agenthook/end") {
						t.Fatalf("%s SessionEnd uses a cold compiler path: %q", name, hook.Command)
					}
					cmd := exec.Command("/bin/sh", "-c", hook.Command)
					cmd.Dir = t.TempDir()
					cmd.Stdin = strings.NewReader(`{"session_id":"outside","cwd":"/"}`)
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("%s %s failed outside a repository: %v\n%s", name, event, err, output)
					}
				}
			}
		}
	}
}

func TestFastEndHookRemovesSessionState(t *testing.T) {
	repo := newGitRepository(t)
	runAgentHook(t, repo, "end-session", "start", false, 0)
	input := `{"session_id":"end-session","cwd":"` + repo + `"}`
	if output, err := runScript(t, repo, filepath.Join(projectRoot(t), "scripts", "agenthook", "end"), input, nil); err != nil {
		t.Fatalf("end hook failed: %v\n%s", err, output)
	}
	stateDir := strings.TrimSpace(git(t, repo, "rev-parse", "--path-format=absolute", "--git-path", "quorum-hook-state"))
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("state directory still contains %d entries", len(entries))
	}
}

func runAgentCommandHook(t *testing.T, repo, sessionID, event string, stopHookActive bool, wantCode int) (string, string) {
	t.Helper()
	input, err := json.Marshal(hookInput{
		SessionID:      sessionID,
		CWD:            repo,
		StopHookActive: stopHookActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runCommandHook([]string{event}, bytes.NewReader(input), &stdout, &stderr); code != wantCode {
		t.Fatalf("runCommandHook(%s) code = %d, want %d; stdout: %s; stderr: %s", event, code, wantCode, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

func TestPreCommitChecksTheIndexWithoutChangingIt(t *testing.T) {
	repo := newGitRepository(t)
	writeTestFile(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	git(t, repo, "add", "main.go")
	git(t, repo, "commit", "-m", "initial")

	writeTestFile(t, repo, "main.go", "package main\nfunc main(){ println(\"staged\") }\n")
	git(t, repo, "add", "main.go")
	writeTestFile(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	indexBefore := git(t, repo, "show", ":main.go")
	worktreeBefore, err := os.ReadFile(filepath.Join(repo, "main.go"))
	if err != nil {
		t.Fatal(err)
	}

	output, err := runScript(t, repo, hookPath(t, "pre-commit"), "", nil)
	if err == nil {
		t.Fatal("pre-commit accepted unformatted staged Go code")
	}
	if !strings.Contains(output, "run gofmt") {
		t.Fatalf("pre-commit output = %q", output)
	}
	if got := git(t, repo, "show", ":main.go"); got != indexBefore {
		t.Fatal("pre-commit changed the index")
	}
	worktreeAfter, err := os.ReadFile(filepath.Join(repo, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(worktreeBefore, worktreeAfter) {
		t.Fatal("pre-commit changed the working tree")
	}

	writeTestFile(t, repo, "main.go", "package main\n\nfunc main() { println(\"staged\") }\n")
	git(t, repo, "add", "main.go")
	if output, err := runScript(t, repo, hookPath(t, "pre-commit"), "", nil); err != nil {
		t.Fatalf("formatted pre-commit failed: %v\n%s", err, output)
	}

	writeTestFile(t, repo, "notes.txt", "trailing whitespace  \n")
	git(t, repo, "add", "notes.txt")
	if output, err := runScript(t, repo, hookPath(t, "pre-commit"), "", nil); err == nil {
		t.Fatal("pre-commit accepted staged trailing whitespace")
	} else if !strings.Contains(output, "trailing whitespace") {
		t.Fatalf("whitespace output = %q", output)
	}
}

func TestPrePushChecksThePushedCommit(t *testing.T) {
	repo := newGitRepository(t)
	logPath := filepath.Join(t.TempDir(), "checks.log")
	writeTestFile(t, repo, "Makefile", ".PHONY: check\ncheck:\n\t@test -z \"$${GIT_DIR:-}\"\n\t@test -z \"$${GIT_WORK_TREE:-}\"\n\t@printf x >> \"$${CHECK_LOG}\"\n\t@test ! -f fail\n")
	git(t, repo, "add", "Makefile")
	git(t, repo, "commit", "-m", "good")
	goodCommit := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	hookEnv := []string{
		"CHECK_LOG=" + logPath,
		"GIT_DIR=" + strings.TrimSpace(git(t, repo, "rev-parse", "--git-dir")),
		"GIT_WORK_TREE=" + repo,
	}

	writeTestFile(t, repo, "fail", "working-tree-only failure\n")
	zero := strings.Repeat("0", 40)
	input := fmt.Sprintf(
		"refs/heads/main %s refs/heads/main %s\nrefs/tags/example %s refs/tags/example %s\n(delete) %s refs/heads/old %s\n",
		goodCommit, zero, goodCommit, zero, zero, goodCommit,
	)
	output, err := runScript(t, repo, hookPath(t, "pre-push"), input, hookEnv)
	if err != nil {
		t.Fatalf("pre-push tested the dirty working tree instead of the commit: %v\n%s", err, output)
	}
	assertFileContents(t, logPath, "x")

	git(t, repo, "add", "fail")
	git(t, repo, "commit", "-m", "bad")
	badCommit := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	if err := os.Remove(filepath.Join(repo, "fail")); err != nil {
		t.Fatal(err)
	}
	input = fmt.Sprintf("refs/heads/main %s refs/heads/main %s\n", badCommit, goodCommit)
	output, err = runScript(t, repo, hookPath(t, "pre-push"), input, hookEnv)
	if err == nil {
		t.Fatal("pre-push accepted a failing pushed commit")
	}
	if !strings.Contains(output, "Push blocked") {
		t.Fatalf("pre-push output = %q", output)
	}

	worktreeList := git(t, repo, "worktree", "list", "--porcelain")
	if got := strings.Count(worktreeList, "worktree "); got != 1 {
		t.Fatalf("pre-push left %d worktrees behind:\n%s", got-1, worktreeList)
	}
}

func runAgentHook(t *testing.T, repo, sessionID, event string, stopHookActive bool, wantCode int) string {
	t.Helper()
	input, err := json.Marshal(hookInput{
		SessionID:      sessionID,
		CWD:            repo,
		StopHookActive: stopHookActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := runHook([]string{event}, bytes.NewReader(input), &stderr); code != wantCode {
		t.Fatalf("runHook(%s) code = %d, want %d; stderr: %s", event, code, wantCode, stderr.String())
	}
	return stderr.String()
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Example User")
	git(t, repo, "config", "user.email", "example@example.com")
	git(t, repo, "config", "commit.gpgSign", "false")
	return repo
}

func hookPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(projectRoot(t), ".githooks", name)
}

func projectRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate hook test")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func runScript(t *testing.T, repo, path, stdin string, extraEnv []string) (string, error) {
	t.Helper()
	cmd := exec.Command(path)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), extraEnv...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeTestFile(t *testing.T, repo, name, contents string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFileMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or stat failed: %v", path, err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != want {
		t.Fatalf("%s = %q, want %q", path, contents, want)
	}
}

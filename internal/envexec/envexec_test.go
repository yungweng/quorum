package envexec

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRunAddsTrimpathToExistingGoFlags(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=readonly")
	var out bytes.Buffer
	err := (Env{Worktree: t.TempDir()}).Run(context.Background(), 0, Cmd{
		Name: "/bin/sh", Args: []string{"-c", `printf %s "$GOFLAGS"`}, Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "-mod=readonly -trimpath"; got != want {
		t.Fatalf("GOFLAGS = %q, want %q", got, want)
	}
}

func TestRunRespectsExplicitTrimpathSetting(t *testing.T) {
	t.Setenv("GOFLAGS", "-trimpath=false")
	var out bytes.Buffer
	err := (Env{Worktree: t.TempDir()}).Run(context.Background(), 0, Cmd{
		Name: "/bin/sh", Args: []string{"-c", `printf %s "$GOFLAGS"`}, Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "-trimpath=false"; got != want {
		t.Fatalf("GOFLAGS = %q, want %q", got, want)
	}
}

func TestRunScopesGitHooksWithoutReplacingExistingCommandConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	hooks := filepath.Join(dir, "private-hooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hooks, err := filepath.EvalSymlinks(hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Example User")
	t.Setenv("EXPECTED_HOOKS", hooks)

	direnv := filepath.Join(t.TempDir(), "direnv")
	direnvScript := `#!/bin/sh
if [ "$1" = allow ]; then
  test "$(git rev-parse --path-format=absolute --git-path hooks/pre-push)" = "$EXPECTED_HOOKS/pre-push"
  exit
fi
shift 2
exec "$@"
`
	if err := os.WriteFile(direnv, []byte(direnvScript), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, env := range []Env{
		{Worktree: dir, GitHooksPath: hooks},
		{Worktree: dir, GitHooksPath: hooks, Direnv: true, DirenvBin: direnv},
	} {
		var out bytes.Buffer
		err = env.Run(context.Background(), 0, Cmd{
			Name:   "/bin/sh",
			Args:   []string{"-c", `printf '%s\n%s' "$(git config user.name)" "$(git rev-parse --path-format=absolute --git-path hooks/pre-push)"`},
			Stdout: &out,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := out.String(), "Example User\n"+filepath.Join(hooks, "pre-push"); got != want {
			t.Fatalf("scoped git config = %q, want %q", got, want)
		}
	}
	if err := (Env{Worktree: dir, GitHooksPath: hooks, DirenvBin: direnv}).Allow(context.Background()); err != nil {
		t.Fatalf("direnv allow did not receive scoped git config: %v", err)
	}
}

func TestRunKeepsGitConfigWritesOutOfTheSharedWorktreeConfig(t *testing.T) {
	repo, worktree := linkedWorktree(t)
	gitDirCmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	gitDirCmd.Dir = worktree
	gitDir, err := gitDirCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	privateConfig := filepath.Join(string(bytes.TrimSpace(gitDir)), "quorum-config")
	sharedConfig, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateConfig, sharedConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	err = (Env{
		Worktree: worktree, GitConfig: privateConfig,
		GitHooksPath: filepath.Join(worktree, "hooks"),
	}).Run(context.Background(), 0, Cmd{
		Name: "/bin/sh", Args: []string{"-c", "git config --local core.hooksPath /dev/null"},
	})
	if err == nil {
		t.Fatal("git config --local succeeded; want the shared scope blocked")
	}
	cmd := exec.Command("git", "config", "--local", "--get", "core.hooksPath")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err == nil || len(out) != 0 {
		t.Fatalf("shared core.hooksPath = %q, %v; want unset", out, err)
	}
	err = (Env{Worktree: worktree, GitConfig: privateConfig}).Run(context.Background(), 0, Cmd{
		Name: "/bin/sh", Args: []string{"-c", "git config core.hooksPath /private/hooks"},
	})
	if err != nil {
		t.Fatalf("unscoped private git config: %v", err)
	}
	cmd = exec.Command("git", "config", "--file", privateConfig, "--get", "core.hooksPath")
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "/private/hooks\n" {
		t.Fatalf("private core.hooksPath = %q, %v; want /private/hooks", out, err)
	}
}

func linkedWorktree(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for key, value := range map[string]string{
		"user.email": "example@example.invalid",
		"user.name":  "Example User",
	} {
		cmd = exec.Command("git", "config", key, value)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", key, err, out)
		}
	}
	cmd = exec.Command("git", "commit", "--quiet", "--allow-empty", "-m", "Initial fixture")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd = exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	return repo, worktree
}

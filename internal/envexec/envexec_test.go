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

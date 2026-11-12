package rundir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/git"
)

// ErrEnvrcChanged means the target touches an .envrc and no override was given.
var ErrEnvrcChanged = errors.New("target changes an .envrc file")

// Isolate gives a checked-out worktree a private snapshot of its hooks and its
// own git config, and returns the environment every command in it runs in.
func Isolate(ctx context.Context, g git.G, worktree, tmpDir string, direnv bool, direnvBin string) (envexec.Env, error) {
	hooksPath, err := g.IsolateHooks(ctx, worktree)
	if err != nil {
		return envexec.Env{}, fmt.Errorf("isolate worktree hooks: %w", err)
	}
	configPath, err := g.IsolateConfig(ctx, worktree)
	if err != nil {
		return envexec.Env{}, fmt.Errorf("isolate worktree config: %w", err)
	}
	return envexec.Env{
		Worktree: worktree, Direnv: direnv, DirenvBin: direnvBin,
		GitConfig: configPath, GitHooksPath: hooksPath, TmpDir: tmpDir,
	}, nil
}

// refuseEnvrcChange fails when the target changed an .envrc relative to
// baseRef and allow is false. Allowing means executing whatever the target's
// author put in that file, on this machine, before any human read it.
func refuseEnvrcChange(ctx context.Context, g git.G, worktree, baseRef string, allow bool) error {
	changed, err := g.ChangedFiles(ctx, worktree, baseRef+"...HEAD", ".envrc", ":(glob)**/.envrc")
	if err != nil {
		return fmt.Errorf("check changed .envrc files: %w", err)
	}
	if changed != "" && !allow {
		return fmt.Errorf("%w:\n%s\nReview the .envrc change manually, then rerun with --allow-envrc-change if it is safe",
			ErrEnvrcChanged, changed)
	}
	return nil
}

// AllowEnvrc runs direnv allow in env's worktree once refuseEnvrcChange
// passed, and reports it through info. fetchBase, when not nil, runs first so
// the caller can bring baseRef up to date; like everything else here it only
// runs when the worktree has an .envrc. It returns the worktree's .envrc
// files; with none, nothing is checked or allowed.
func AllowEnvrc(ctx context.Context, g git.G, env envexec.Env, baseRef string, allow bool,
	fetchBase func() error, info func(string),
) ([]string, error) {
	files := FindEnvrc(env.Worktree)
	if len(files) == 0 {
		return nil, nil
	}
	if fetchBase != nil {
		if err := fetchBase(); err != nil {
			return nil, err
		}
	}
	if err := refuseEnvrcChange(ctx, g, env.Worktree, baseRef, allow); err != nil {
		return nil, err
	}
	if err := env.Allow(ctx); err != nil {
		return nil, fmt.Errorf("direnv allow: %w", err)
	}
	info("direnv allowed")
	return files, nil
}

func FindEnvrc(root string) []string {
	var out []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is not an answer
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == ".envrc" {
			if rel, err := filepath.Rel(root, path); err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// IsolateConfig copies the repository config into the linked worktree's
// private Git directory. GIT_CONFIG can point at the copy so agent-run
// `git config` commands cannot write to the repository's shared config.
func (g G) IsolateConfig(ctx context.Context, worktree string) (string, error) {
	plain := G{Bin: g.Bin}
	gitDir, err := plain.run(ctx, worktree, "rev-parse", "--path-format=absolute", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	commonDir, err := plain.run(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	target := filepath.Join(gitDir, "quorum-config")
	if ready, err := privateConfigReady(target); ready || err != nil {
		return target, err
	}
	data, err := os.ReadFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return "", err
	}
	if err := publishConfig(data, gitDir, target); err != nil {
		if ready, statErr := privateConfigReady(target); ready && statErr == nil {
			return target, nil
		}
		return "", err
	}
	return target, nil
}

func publishConfig(data []byte, gitDir, target string) error {
	tmp, err := os.CreateTemp(gitDir, ".quorum-config-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("publish isolated config: %w", err)
	}
	return nil
}

func privateConfigReady(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("isolated config is not a regular file: %s", path)
	}
	return true, nil
}

// IsolateHooks snapshots the worktree's configured hooks into its private Git
// directory. Linked worktrees otherwise share hooks, so one environment's hook
// installer can rewrite another run's verification while it is being checked.
func (g G) IsolateHooks(ctx context.Context, worktree string) (string, error) {
	plain := G{Bin: g.Bin}
	source, err := plain.run(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	gitDir, err := plain.run(ctx, worktree, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	target := filepath.Join(gitDir, "quorum-hooks")
	if ready, err := hookSnapshotReady(target); ready || err != nil {
		return target, err
	}
	tmp, err := os.MkdirTemp(gitDir, ".quorum-hooks-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := copyHookTree(source, tmp); err != nil {
		return "", fmt.Errorf("snapshot hooks from %s: %w", source, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		if ready, statErr := hookSnapshotReady(target); ready && statErr == nil {
			return target, nil
		}
		return "", fmt.Errorf("publish isolated hooks: %w", err)
	}
	return target, nil
}

func hookSnapshotReady(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("isolated hooks path is not a directory: %s", path)
	}
	return true, nil
}

func copyHookTree(source, target string) error {
	resolved, err := filepath.EvalSymlinks(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(resolved, path)
		if err != nil {
			return err
		}
		return copyHookEntry(resolved, target, path, filepath.Join(target, rel), entry)
	})
}

func copyHookEntry(sourceRoot, targetRoot, source, target string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if entry.IsDir() {
		return os.MkdirAll(target, info.Mode().Perm())
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return copyHookLink(sourceRoot, targetRoot, source, target)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported hook file type: %s", source)
	}
	return copyHookFile(source, target, info.Mode().Perm())
}

func copyHookLink(sourceRoot, targetRoot, source, target string) error {
	link, err := os.Readlink(source)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(source), link)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return err
	}
	if rel, inside := relativeInside(sourceRoot, resolved); inside {
		mapped := filepath.Join(targetRoot, rel)
		privateLink, err := filepath.Rel(filepath.Dir(target), mapped)
		if err != nil {
			return err
		}
		return os.Symlink(privateLink, target)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyHookTree(resolved, target)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported hook symlink target: %s", source)
	}
	return copyHookFile(resolved, target, info.Mode().Perm())
}

func relativeInside(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", false
	}
	return rel, true
}

func copyHookFile(source, target string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

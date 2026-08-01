// Package cachefs owns the destructive filesystem operations used for cache
// entries. Dependency managers deliberately make some caches read-only, so a
// plain os.RemoveAll is not sufficient for directories quorum owns.
package cachefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll removes a cache entry, retrying after making the remaining tree
// owner-writable. Go's module cache uses read-only directories; removing a
// file from one fails even when every path belongs to the current user.
//
// WalkDir does not follow symlinks, so making a cached worktree removable can
// never chmod a shared dependency tree linked from it.
func RemoveAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if chmodErr := makeWritable(path); chmodErr != nil {
		return errors.Join(err, chmodErr)
	}
	return os.RemoveAll(path)
}

func makeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		mode := info.Mode().Perm()
		if entry.IsDir() {
			mode |= 0o700
		} else if info.Mode().IsRegular() {
			mode |= 0o600
		} else {
			return nil
		}
		return os.Chmod(path, mode)
	})
}

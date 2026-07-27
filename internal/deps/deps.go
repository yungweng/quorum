// Package deps shares installed dependency trees between review runs.
//
// A project that installs its dependencies from a direnv or devbox hook pays
// for that install once per worktree, and the hook's own "is it already there?"
// guard is no help when six reviewers enter the environment in the same second:
// all six see an empty directory and all six install. So the environment is
// entered exactly once, and whatever the hook installed is moved into a cache
// keyed by the hash of the lock file that produced it. The next run links the
// tree back in, the hook's guard sees a populated directory, and nothing
// installs at all.
//
// Any change to the lock file produces a different hash, so a stale tree is
// never reused.
package deps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// lockFiles are the manifests that mark a directory as a JavaScript project.
// All of them install into node_modules, which is what makes one shared
// directory name enough.
var lockFiles = []string{
	"pnpm-lock.yaml",
	"package-lock.json",
	"yarn.lock",
	"bun.lock",
	"bun.lockb",
}

// pruneDirs are never descended into while looking for lock files.
var pruneDirs = []string{"node_modules", ".git", "vendor"}

// maxDepth bounds the search below the worktree root. Three levels covers the
// usual monorepo shapes (root, apps/web, packages/*/) without walking a tree
// that may hold a full checkout of everything.
const maxDepth = 3

// completeMarker is written only after a tree has been fully published, so a
// half-moved tree left behind by a killed run is never linked into a worktree.
const completeMarker = ".complete"

// staleLock is when a publish lock is assumed to belong to a run that died
// holding it.
const staleLock = time.Hour

// Cache is the shared dependency cache for one repository and worktree.
type Cache struct {
	Root     string // the deps cache root, one level above the per-repo dirs
	Repo     string // repository name with the slash replaced, e.g. "owner-repo"
	Worktree string
}

// Project is one directory inside the worktree that owns a lock file, together
// with the hash of that lock file.
type Project struct {
	Rel  string // relative to the worktree, "." for the root
	Hash string
}

// Projects lists every directory in the worktree that owns a lock file.
// A directory holding both a pnpm and an npm lock file is reported once; the
// first lock file found wins, matching the order of lockFiles.
func (c Cache) Projects() ([]Project, error) {
	seen := map[string]bool{}
	var out []Project

	err := filepath.WalkDir(c.Worktree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not worth failing a review over.
			return nil //nolint:nilerr // deliberate: skip and keep walking
		}
		if d.IsDir() {
			if path == c.Worktree {
				return nil
			}
			if slices.Contains(pruneDirs, d.Name()) {
				return filepath.SkipDir
			}
			if depth(c.Worktree, path) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !slices.Contains(lockFiles, d.Name()) {
			return nil
		}
		dir := filepath.Dir(path)
		if seen[dir] {
			return nil
		}
		seen[dir] = true
		hash, err := hashFile(path)
		if err != nil {
			return nil //nolint:nilerr // an unreadable lock file just means no sharing
		}
		out = append(out, Project{Rel: relTo(c.Worktree, dir), Hash: hash})
		return nil
	})
	return out, err
}

// Link symlinks every cached tree that matches a project's current lock file
// into the worktree, and returns the links it created plus the projects it
// reused. A project whose node_modules already exists is left alone.
func (c Cache) Link() (links []string, reused []Project, err error) {
	projects, err := c.Projects()
	if err != nil {
		return nil, nil, err
	}
	for _, p := range projects {
		shared := c.dir(p)
		marker := filepath.Join(shared, completeMarker)
		if !exists(marker) {
			continue
		}
		target := filepath.Join(c.projectDir(p), "node_modules")
		// Follows symlinks on purpose: anything already sitting there, real
		// directory or link, means there is nothing to do.
		if exists(target) {
			continue
		}
		if err := os.Symlink(shared, target); err != nil {
			continue
		}
		links = append(links, target)
		reused = append(reused, p)
		touch(marker)
	}
	return links, reused, nil
}

// Capture publishes dependency trees that the environment hook just installed,
// and links the worktree at the published copy. It returns the links it created
// and the projects it newly cached.
//
// A project whose node_modules is already a symlink is pointing at the shared
// tree and has nothing to contribute.
func (c Cache) Capture() (links []string, cached []Project, err error) {
	projects, err := c.Projects()
	if err != nil {
		return nil, nil, err
	}
	for _, p := range projects {
		local := filepath.Join(c.projectDir(p), "node_modules")
		info, lerr := os.Lstat(local)
		if lerr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}

		shared := c.dir(p)
		if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
			continue
		}

		lock := shared + ".lock"
		if lockIsStale(lock) {
			os.RemoveAll(lock)
		}
		// A directory is the atomic primitive here: mkdir either creates it or
		// fails, with no window in between. Failing means another run is
		// publishing the same tree right now; ours stays where it is, so the
		// run still works, it just does not get shared this time.
		if err := os.Mkdir(lock, 0o755); err != nil {
			continue
		}

		marker := filepath.Join(shared, completeMarker)
		if exists(marker) {
			// Another run published it while the hook was still installing
			// ours. Theirs is as good as ours, so drop the duplicate.
			os.RemoveAll(local)
		} else {
			os.RemoveAll(shared)
			if err := os.Rename(local, shared); err != nil {
				// Crossing a filesystem boundary is the usual cause. Nothing
				// was lost: the tree is still where the hook installed it.
				os.RemoveAll(lock)
				continue
			}
			touch(marker)
			cached = append(cached, p)
		}

		if err := os.Symlink(shared, local); err == nil {
			links = append(links, local)
		}
		os.RemoveAll(lock)
	}
	return links, cached, nil
}

// Unlink removes the symlinks a run created. Removing a symlink never touches
// its target, but the shared trees are expensive enough to not want that
// resting on it, so they go before anything deletes the worktree.
func Unlink(links []string) {
	for _, l := range links {
		if info, err := os.Lstat(l); err == nil && info.Mode()&os.ModeSymlink != 0 {
			os.Remove(l)
		}
	}
}

// Tree is one shared dependency tree in the cache, with the last time a run
// needed it.
type Tree struct {
	Path string
	Used time.Time
}

// Trees lists every tree in the cache, least recently used first.
//
// Use is read from the .complete marker, which Link touches on every reuse.
// The shell version stat'ed the directory instead, which does not work: writing
// a file inside a directory leaves the directory's own mtime untouched, so a
// tree that had been reused daily since it was created still looked untouched
// and was deleted on schedule. Verified on macOS; the marker's mtime is the
// value that actually tracks use.
//
// A tree with no marker is a publish that never finished. It sorts first,
// because it is dead weight whatever the caller is deleting for.
func (c Cache) Trees() []Tree {
	// <root>/<repo>/<project>/<hash>, the layout dir writes.
	matches, _ := filepath.Glob(filepath.Join(c.Root, "*", "*", "*"))
	out := make([]Tree, 0, len(matches))
	for _, tree := range matches {
		t := Tree{Path: tree}
		// The marker doubles as the check that this is a tree at all: nothing
		// else at this depth can hold one.
		if info, err := os.Stat(filepath.Join(tree, completeMarker)); err == nil {
			t.Used = info.ModTime()
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Tree) int { return a.Used.Compare(b.Used) })
	return out
}

// GC removes shared trees that no run has needed for maxAge, and returns how
// many it deleted.
func (c Cache) GC(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, t := range c.Trees() {
		// Ordered by use, so the first tree still in date ends the sweep.
		if !t.Used.Before(cutoff) {
			break
		}
		if os.RemoveAll(t.Path) == nil {
			removed++
		}
	}
	return removed
}

// dir is where a project's tree lives in the cache: <root>/<repo>/<path>/<hash>.
// The hash in the leaf is what makes a changed lock file miss the cache
// instead of reusing a tree that no longer matches it.
func (c Cache) dir(p Project) string {
	return filepath.Join(c.Root, c.Repo, slug(p.Rel), p.Hash)
}

// slug turns a project path into one path element.
//
// The root project needs its own name rather than ".": path joining collapses
// a "." component away, which would put the root project's tree one level
// higher than every other project's and break any layer that reasons about the
// cache by depth, the GC included.
func slug(rel string) string {
	if rel == "." || rel == "" {
		return "_root"
	}
	return strings.ReplaceAll(rel, "/", "_")
}

func (c Cache) projectDir(p Project) string {
	if p.Rel == "." {
		return c.Worktree
	}
	return filepath.Join(c.Worktree, p.Rel)
}

// hashFile returns the first 12 hex characters of a file's SHA-256. Twelve is
// what the shell version used: 48 bits, against a handful of lock files per
// repository.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func lockIsStale(lock string) bool {
	info, err := os.Stat(lock)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > staleLock
}

func depth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

func relTo(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "" {
		return "."
	}
	return filepath.ToSlash(rel)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func touch(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		if f, cerr := os.Create(path); cerr == nil {
			f.Close()
		}
	}
}

// String renders a project the way the run header prints it.
func (p Project) String() string { return fmt.Sprintf("%s (%s)", p.Rel, p.Hash) }

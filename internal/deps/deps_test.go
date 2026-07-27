package deps

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newCache builds a cache over a fresh worktree.
func newCache(t *testing.T) Cache {
	t.Helper()
	root := t.TempDir()
	return Cache{
		Root:     filepath.Join(root, "deps"),
		Repo:     "acme-api",
		Worktree: mustDir(t, filepath.Join(root, "worktree")),
	}
}

func mustDir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mustDir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectsFindsEveryLockFileDirectory(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "root")
	writeFile(t, filepath.Join(c.Worktree, "apps", "web", "package-lock.json"), "web")

	got, err := c.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d projects, want 2: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		seen[p.Rel] = true
		if len(p.Hash) != 12 {
			t.Errorf("hash %q is not 12 characters", p.Hash)
		}
	}
	if !seen["."] || !seen["apps/web"] {
		t.Errorf("wrong project paths: %+v", got)
	}
}

// A directory holding two lock files must be reported once, or its tree would
// be published twice under different hashes.
func TestProjectsReportsADirectoryOnce(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "a")
	writeFile(t, filepath.Join(c.Worktree, "package-lock.json"), "b")

	got, err := c.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a directory with two lock files produced %d projects", len(got))
	}
}

// node_modules can contain thousands of nested lock files. Walking into it
// would be slow and would report dependencies as projects of their own.
func TestProjectsSkipsNodeModules(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "root")
	writeFile(t, filepath.Join(c.Worktree, "node_modules", "left-pad", "package-lock.json"), "dep")

	got, err := c.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Rel != "." {
		t.Errorf("walked into node_modules: %+v", got)
	}
}

// A changed lock file must miss the cache, or a run would reuse a tree that no
// longer matches what the project declares.
func TestChangedLockFileGetsADifferentHash(t *testing.T) {
	c := newCache(t)
	lock := filepath.Join(c.Worktree, "pnpm-lock.yaml")

	writeFile(t, lock, "version: 1")
	before, _ := c.Projects()
	writeFile(t, lock, "version: 2")
	after, _ := c.Projects()

	if before[0].Hash == after[0].Hash {
		t.Error("a changed lock file produced the same hash, so a stale tree would be reused")
	}
}

// The round trip: a run publishes what the hook installed, the next run links
// it back in instead of installing again.
func TestCaptureThenLinkReusesTheTree(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "v1")
	writeFile(t, filepath.Join(c.Worktree, "node_modules", "left-pad", "index.js"), "module")

	links, cached, err := c.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 {
		t.Fatalf("captured %d projects, want 1", len(cached))
	}
	if len(links) != 1 {
		t.Fatalf("capture created %d links, want 1", len(links))
	}
	// The worktree now points at the shared copy rather than holding its own.
	info, err := os.Lstat(filepath.Join(c.Worktree, "node_modules"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("node_modules is not a symlink after capture")
	}

	// A second worktree with the same lock file reuses it without installing.
	next := c
	next.Worktree = mustDir(t, filepath.Join(t.TempDir(), "worktree2"))
	writeFile(t, filepath.Join(next.Worktree, "pnpm-lock.yaml"), "v1")

	links, reused, err := next.Link()
	if err != nil {
		t.Fatal(err)
	}
	if len(reused) != 1 {
		t.Fatalf("reused %d trees, want 1", len(reused))
	}
	if _, err := os.Stat(filepath.Join(next.Worktree, "node_modules", "left-pad", "index.js")); err != nil {
		t.Errorf("the linked tree does not contain the installed files: %v", err)
	}

	// Dropping the links must never touch the shared tree.
	Unlink(links)
	if _, err := os.Stat(filepath.Join(next.Worktree, "node_modules")); !os.IsNotExist(err) {
		t.Error("the symlink survived Unlink")
	}
	if _, err := os.Stat(filepath.Join(c.dir(cached[0]), "left-pad", "index.js")); err != nil {
		t.Errorf("Unlink damaged the shared tree: %v", err)
	}
}

// A tree without its completion marker was left by a run that died mid-publish
// and must never be linked into a worktree.
func TestLinkIgnoresAnIncompleteTree(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "v1")

	projects, _ := c.Projects()
	half := c.dir(projects[0])
	writeFile(t, filepath.Join(half, "left-pad", "index.js"), "half written")

	links, reused, err := c.Link()
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 || len(reused) != 0 {
		t.Error("an unfinished tree was linked into the worktree")
	}
}

// Existing dependencies win: linking over them would hide what is really there.
func TestLinkLeavesAnExistingNodeModulesAlone(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "v1")
	writeFile(t, filepath.Join(c.Worktree, "node_modules", "left-pad", "index.js"), "local")

	projects, _ := c.Projects()
	shared := c.dir(projects[0])
	writeFile(t, filepath.Join(shared, completeMarker), "")

	links, _, err := c.Link()
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Error("an existing node_modules was replaced by a symlink")
	}
}

// The regression this port exists to fix.
//
// The shell version touched the marker file on every reuse but aged the
// *directory*, whose mtime a write inside it never changes. A tree reused daily
// therefore still looked untouched and was deleted on schedule, costing a full
// reinstall every fortnight. GC must age the marker.
func TestGCKeepsATreeThatIsStillBeingUsed(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "v1")
	writeFile(t, filepath.Join(c.Worktree, "node_modules", "left-pad", "index.js"), "module")

	_, cached, err := c.Capture()
	if err != nil || len(cached) != 1 {
		t.Fatalf("capture failed: %v", err)
	}
	tree := c.dir(cached[0])

	// Age the directory itself well past the retention window, exactly as a
	// tree created a month ago would be, but keep the marker fresh: the tree
	// was used a minute ago.
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(tree, old, old); err != nil {
		t.Fatal(err)
	}
	touch(filepath.Join(tree, completeMarker))

	if removed := c.GC(14 * 24 * time.Hour); removed != 0 {
		t.Errorf("GC removed %d trees that were still in use", removed)
	}
	if _, err := os.Stat(filepath.Join(tree, "left-pad", "index.js")); err != nil {
		t.Error("a tree in active use was deleted")
	}
}

func TestGCRemovesATreeNobodyNeeded(t *testing.T) {
	c := newCache(t)
	writeFile(t, filepath.Join(c.Worktree, "pnpm-lock.yaml"), "v1")
	writeFile(t, filepath.Join(c.Worktree, "node_modules", "left-pad", "index.js"), "module")

	_, cached, _ := c.Capture()
	tree := c.dir(cached[0])

	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(tree, completeMarker), old, old); err != nil {
		t.Fatal(err)
	}
	if removed := c.GC(14 * 24 * time.Hour); removed != 1 {
		t.Errorf("GC removed %d trees, want 1", removed)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Error("an unused tree survived the GC")
	}
}

// A publish that never completed is dead weight regardless of its age.
func TestGCRemovesAnIncompleteTree(t *testing.T) {
	c := newCache(t)
	half := filepath.Join(c.Root, c.Repo, "_root", "abc123456789")
	writeFile(t, filepath.Join(half, "left-pad", "index.js"), "half")

	if removed := c.GC(14 * 24 * time.Hour); removed != 1 {
		t.Errorf("GC removed %d trees, want 1", removed)
	}
}

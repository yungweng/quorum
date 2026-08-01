package cachefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAllHandlesReadOnlyDependencyTrees(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktree")
	module := filepath.Join(root, ".devbox", "go", "pkg", "mod", "example.org", "module@v1.0.0")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "source.go"), []byte("package module\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(module, 0o555); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("removed tree still exists: %v", err)
	}
}

func TestRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "shared")
	if err := os.Mkdir(target, 0o555); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "worktree")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "node_modules")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("shared target was removed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o555 {
		t.Fatalf("shared target mode = %o, want 555", got)
	}
}

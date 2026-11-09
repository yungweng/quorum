package runtmp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/proc"
)

func TestNewCreatesClaimedDirInTempDirAndRemoveDeletesIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)

	dir, remove, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != root {
		t.Fatalf("dir %s is not directly in %s", dir, root)
	}
	if !proc.Claimed(dir) {
		t.Fatal("new temp dir is not claimed")
	}
	// Leaked trees can be read-only, like Go's module cache.
	locked := filepath.Join(dir, "nix-1-2", "lib")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}

	remove()
	remove()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir still exists after remove: %v", err)
	}
}

func TestSweepRemovesOnlyUnclaimedQuorumDirs(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, claimFile bool) string {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if claimFile {
			if err := os.WriteFile(filepath.Join(dir, proc.ClaimFile), []byte("1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	orphan := mk(Prefix+"orphan", true)
	live := mk(Prefix+"live", false)
	release, err := proc.Claim(live)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	unclaimed := mk(Prefix+"no-claim-file", false)
	other := mk("other-tool", true)

	if got := sweep(root); got != 1 {
		t.Fatalf("sweep removed %d dirs, want 1", got)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan survived: %v", err)
	}
	for _, dir := range []string{live, unclaimed, other} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s was removed: %v", dir, err)
		}
	}
}

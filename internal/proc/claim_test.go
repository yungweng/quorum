package proc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaimed(t *testing.T) {
	dir := t.TempDir()

	if Claimed(dir) {
		t.Error("an unclaimed directory reads as held, which would make it uncollectable forever")
	}

	if err := Claim(dir); err != nil {
		t.Fatal(err)
	}
	if !Claimed(dir) {
		t.Error("a directory claimed by this very process reads as free")
	}

	// Above PID_MAX on macOS and the default pid_max on Linux: a claim no
	// running process can answer for, which is what a killed run leaves.
	if err := os.WriteFile(filepath.Join(dir, ClaimFile), []byte("2147483647\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Claimed(dir) {
		t.Error("a claim from a process that is gone still reads as held")
	}

	if err := os.WriteFile(filepath.Join(dir, ClaimFile), []byte("not a pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Claimed(dir) {
		t.Error("an unreadable claim reads as held")
	}
}

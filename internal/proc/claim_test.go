package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestClaimed(t *testing.T) {
	dir := t.TempDir()

	if Claimed(dir) {
		t.Error("an unclaimed directory reads as held, which would make it uncollectable forever")
	}

	release, err := Claim(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Claimed(dir) {
		t.Error("a directory claimed by this very process reads as free")
	}
	release()
	if Claimed(dir) {
		t.Error("a released directory still reads as held")
	}

	// An unlocked file is stale even when it names this process. A PID-only
	// check would mistake it for a live claim, and PID reuse makes that happen
	// to real run directories eventually.
	if err := os.WriteFile(filepath.Join(dir, ClaimFile), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if Claimed(dir) {
		t.Error("an unlocked claim with a live PID reads as held")
	}

	if err := os.WriteFile(filepath.Join(dir, ClaimFile), []byte("not a pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Claimed(dir) {
		t.Error("an unreadable claim reads as held")
	}
}

func TestClaimRefusesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	release, err := Claim(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if second, err := Claim(dir); err == nil {
		second()
		t.Error("a second process instance could replace a live claim")
	}
}

func TestClaimReleaseKeepsTheInodeForAResumedClaim(t *testing.T) {
	dir := t.TempDir()
	release, err := Claim(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ClaimFile)
	collector, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()

	release()
	resumedRelease, err := Claim(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resumedRelease()

	if err := syscall.Flock(int(collector.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(collector.Fd()), syscall.LOCK_UN)
		t.Error("a reader of the released claim missed the resumed claim on a different inode")
	}
}

func TestLockDirCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "cache")
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("lock directory was not created: %v", err)
	}
}

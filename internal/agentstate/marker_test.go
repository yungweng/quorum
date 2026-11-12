package agentstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMarkerIsExclusive(t *testing.T) {
	dir := t.TempDir()
	key := "acme/api#1"

	m, got, err := Acquire(dir, key)
	if err != nil || !got {
		t.Fatalf("first Acquire: got=%v err=%v", got, err)
	}
	// This process is alive, so a second claim must fail.
	if _, got, _ := Acquire(dir, key); got {
		t.Error("the same review was claimed twice")
	}
	if live := Live(dir); len(live) != 1 || live[0].Key != key {
		t.Errorf("Live = %+v", live)
	}
	if keys := LiveKeys(dir); len(keys) != 1 || !keys[key] {
		t.Errorf("LiveKeys = %v", keys)
	}
	m.Release()
	if live := Live(dir); len(live) != 0 {
		t.Errorf("marker survived Release: %+v", live)
	}
}

func TestMarkerStaleIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	key := "acme/api#2"
	// pid 0 is never a live process here, so this stands in for a crashed run.
	path := filepath.Join(dir, safeName(key))
	if err := os.WriteFile(path, []byte("0\n"+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if live := Live(dir); len(live) != 0 {
		t.Error("a dead marker counted as a running review")
	}
	if err := os.WriteFile(path, []byte("0\n"+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, got, err := Acquire(dir, key); !got || err != nil {
		t.Errorf("a stale marker blocked a new review: got=%v err=%v", got, err)
	}
}

func TestMarkerAdopt(t *testing.T) {
	dir := t.TempDir()
	m, _, err := Acquire(dir, "acme/api#3")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Adopt(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	live := Live(dir)
	if len(live) != 1 || live[0].PID != os.Getpid() || live[0].Key != "acme/api#3" {
		t.Errorf("Live = %+v", live)
	}
}

func TestLoadAvgIsPlausible(t *testing.T) {
	load, ok := LoadAvg1()
	if !ok {
		t.Skip("no load average on this platform")
	}
	if load < 0 || load > 1000 {
		t.Errorf("implausible load average: %v", load)
	}
}

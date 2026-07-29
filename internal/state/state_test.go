package state

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The file the shell version wrote stores every number as a string and carries
// fields that no longer exist. It has to load without a migration step.
func TestReadShellEraState(t *testing.T) {
	legacy := `{
  "schema": 1,
  "prs": {
    "Crumb-GmbH/waffle-iron#103": {
      "day": "2026-07-25",
      "count": "1",
      "sha": "9bce811de6eed61345735d2b278b21670af46cf5",
      "status": "ok",
      "at": "2026-07-25T15:43:21",
      "comment_url": "https://github.com/Crumb-GmbH/waffle-iron/pull/103#issuecomment-5078706902",
      "blockers": "0",
      "critical": "4",
      "runlog": "/Users/x/.local/state/prbot/runs/waffle-iron.log",
      "req_at": "2026-07-16T12:50:30Z",
      "fails": "0"
    },
    "crumbtray/toaster-api#1993": {
      "status": "failed",
      "at": "2026-07-25T22:49:25",
      "fails": "2"
    }
  }
}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	waffle := f.PRs["Crumb-GmbH/waffle-iron#103"]
	if waffle.Critical != 4 || waffle.Blockers != 0 || waffle.Fails != 0 {
		t.Errorf("string numbers did not parse: %+v", waffle)
	}
	if waffle.Status != OK {
		t.Errorf("status = %q", waffle.Status)
	}
	if f.PRs["crumbtray/toaster-api#1993"].Fails != 2 {
		t.Error("fails did not parse")
	}

	entries := f.Entries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	// Newest change first.
	if entries[0].Key != "crumbtray/toaster-api#1993" {
		t.Errorf("wrong order: %s first", entries[0].Key)
	}
	// The zone-less timestamps of the old file still have to parse.
	if entries[0].Time().IsZero() {
		t.Error("legacy timestamp did not parse")
	}
}

func TestEntryKeyParts(t *testing.T) {
	e := Entry{Key: "crumbtray/toaster-api#2017"}
	if e.Repo() != "crumbtray/toaster-api" {
		t.Errorf("Repo = %q", e.Repo())
	}
	if e.Name() != "toaster-api" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Number() != 2017 {
		t.Errorf("Number = %d", e.Number())
	}
	if e.URL() != "https://github.com/crumbtray/toaster-api/pull/2017" {
		t.Errorf("URL = %q", e.URL())
	}
}

func TestReadMissingFile(t *testing.T) {
	f, err := Read(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing state should not be an error: %v", err)
	}
	if len(f.PRs) != 0 {
		t.Error("missing state was not empty")
	}
}

func TestMutateIsAtomicUnderConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const writers = 12
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every writer bumps the same counter, so a lost update shows up as
			// a total below the writer count.
			if err := Mutate(path, "acme/api#1", func(r *Record) { r.Fails++ }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.PRs["acme/api#1"].Fails; got != writers {
		t.Errorf("Fails = %d after %d concurrent writers, want %d", got, writers, writers)
	}
}

// A lock directory left behind by the shell version must not wedge the Go one.
func TestMutateClearsLegacyLockDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path+".lock", 0o755); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Mutate(path, "acme/api#2", func(r *Record) { r.Status = Running }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Mutate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Mutate blocked on the stale lock directory")
	}
	f, _ := Read(path)
	if f.PRs["acme/api#2"].Status != Running {
		t.Error("record not written")
	}
}

func TestWriteTrimsToNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f := File{Schema: 1, PRs: map[string]Record{}}
	base := time.Now().Add(-time.Duration(keep+50) * time.Hour)
	for i := range keep + 50 {
		f.PRs[key(i)] = Record{
			Status: OK,
			At:     base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
		}
	}
	if err := write(path, f); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PRs) != keep {
		t.Errorf("kept %d records, want %d", len(got.PRs), keep)
	}
	// The newest record must be among the survivors, the oldest must not.
	if _, ok := got.PRs[key(keep+49)]; !ok {
		t.Error("newest record was trimmed")
	}
	if _, ok := got.PRs[key(0)]; ok {
		t.Error("oldest record survived the trim")
	}
}

func key(i int) string {
	return "acme/api#" + strconv.Itoa(i)
}

// Caching a timeline lookup must not make a finished review look fresh. Only a
// real change of state moves the timestamp.
func TestBookkeepingDoesNotTouchTheTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	finished := "2026-07-25T15:43:21+02:00"
	if err := Mutate(path, "acme/api#1", func(r *Record) {
		r.Mark(OK, "")
		r.At = finished
	}); err != nil {
		t.Fatal(err)
	}

	// A poll that only caches what it learned from the timeline.
	if err := Mutate(path, "acme/api#1", func(r *Record) {
		r.TimelineAt = "2026-07-26T21:43:16Z"
		r.SeenReqAt = "2026-07-26T20:31:44Z"
	}); err != nil {
		t.Fatal(err)
	}
	f, _ := Read(path)
	if got := f.PRs["acme/api#1"].At; got != finished {
		t.Errorf("bookkeeping moved the timestamp to %q, want %q", got, finished)
	}

	// A real transition does move it.
	if err := Mutate(path, "acme/api#1", func(r *Record) { r.Mark(Running, "") }); err != nil {
		t.Fatal(err)
	}
	f, _ = Read(path)
	if f.PRs["acme/api#1"].At == finished {
		t.Error("a change of state left the timestamp behind")
	}
	if f.PRs["acme/api#1"].Status != Running {
		t.Error("Mark did not set the status")
	}
}

func TestMarkSetsReason(t *testing.T) {
	var r Record
	r.Mark(Skipped, "draft")
	if r.Status != Skipped || r.Reason != "draft" || r.At == "" {
		t.Errorf("Mark left the record as %+v", r)
	}
	r.Mark(Running, "")
	if r.Reason != "" {
		t.Errorf("Mark did not clear the stale reason: %q", r.Reason)
	}
}

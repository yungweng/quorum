package state

import (
	"encoding/json"
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
    "acme/api#42": {
      "day": "2026-07-25",
      "count": "1",
      "sha": "0123456789abcdef0123456789abcdef01234567",
      "status": "ok",
      "at": "2026-07-25T15:43:21",
      "comment_url": "https://github.com/acme/api/pull/42#issuecomment-1",
      "blockers": "0",
      "critical": "4",
      "runlog": "/tmp/example-user/prbot/runs/api.log",
      "req_at": "2026-07-16T12:50:30Z",
      "fails": "0"
    },
    "acme/web#43": {
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
	api := f.PRs["acme/api#42"]
	if api.Critical != 4 || api.Blockers != 0 || api.Fails != 0 {
		t.Errorf("string numbers did not parse: %+v", api)
	}
	if api.Status != OK {
		t.Errorf("status = %q", api.Status)
	}
	if f.PRs["acme/web#43"].Fails != 2 {
		t.Error("fails did not parse")
	}

	entries := f.Entries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	// Newest change first.
	if entries[0].Key != "acme/web#43" {
		t.Errorf("wrong order: %s first", entries[0].Key)
	}
	// The zone-less timestamps of the old file still have to parse.
	if entries[0].Time().IsZero() {
		t.Error("legacy timestamp did not parse")
	}
}

func TestEntryKeyParts(t *testing.T) {
	e := Entry{Key: "acme/api#42"}
	if e.Repo() != "acme/api" {
		t.Errorf("Repo = %q", e.Repo())
	}
	if e.Name() != "api" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Number() != 42 {
		t.Errorf("Number = %d", e.Number())
	}
	if e.URL() != "https://github.com/acme/api/pull/42" {
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

// Every write trims the file to the newest records, so it cannot grow without
// bound across months of polling.
func TestMutateTrimsToNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f := File{Schema: 1, PRs: map[string]Record{}}
	base := time.Now().Add(-time.Duration(keep+50) * time.Hour)
	for i := range keep + 50 {
		f.PRs[key(i)] = Record{
			Status: OK,
			At:     base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
		}
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Mutate(path, "acme/web#7", func(r *Record) { r.Mark(Running, "") }); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PRs) != keep {
		t.Errorf("kept %d records, want %d", len(got.PRs), keep)
	}
	// The record just written and the newest old one survive, the oldest does not.
	for _, k := range []string{"acme/web#7", key(keep + 49)} {
		if _, ok := got.PRs[k]; !ok {
			t.Errorf("newest record %s was trimmed", k)
		}
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

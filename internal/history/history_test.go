package history

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func path(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "history.jsonl")
}

func TestReadReturnsTheNewestFirst(t *testing.T) {
	p := path(t)
	base := time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC)
	for i, key := range []string{"acme/api#1", "acme/api#2", "acme/web#3"} {
		if err := Append(p, Run{
			Key: key, Kind: KindReview, Source: SourceManual, Outcome: OK,
			EndedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := Read(p, 0)
	if len(got) != 3 {
		t.Fatalf("read %d runs, want 3", len(got))
	}
	if got[0].Key != "acme/web#3" || got[2].Key != "acme/api#1" {
		t.Errorf("wrong order: %s ... %s", got[0].Key, got[2].Key)
	}
	// The same pull request twice has to stay two entries. Collapsing them is
	// exactly what the state file already does and the reason this exists.
	if err := Append(p, Run{Key: "acme/api#1", Kind: KindReview, Outcome: Failed}); err != nil {
		t.Fatal(err)
	}
	if got := Read(p, 0); len(got) != 4 {
		t.Errorf("a second run on the same pull request was merged away: %d entries", len(got))
	}
}

func TestReadHonoursTheLimit(t *testing.T) {
	p := path(t)
	for i := range 5 {
		if err := Append(p, Run{Key: "acme/api#1", Outcome: OK, EndedAt: time.Now().Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := Read(p, 2); len(got) != 2 {
		t.Errorf("limit 2 returned %d", len(got))
	}
	if got := Read(p, 99); len(got) != 5 {
		t.Errorf("a limit above the log size returned %d, want everything", len(got))
	}
}

func TestAppendTrimsToTheCap(t *testing.T) {
	p := path(t)
	for i := range maxEntries + 20 {
		if err := Append(p, Run{Key: "acme/api#1", Outcome: OK, Rounds: i}); err != nil {
			t.Fatal(err)
		}
	}
	got := Read(p, 0)
	if len(got) != maxEntries {
		t.Fatalf("log holds %d entries, want the cap %d", len(got), maxEntries)
	}
	// Trimming must drop the oldest, not the newest.
	if got[0].Rounds != maxEntries+19 {
		t.Errorf("newest entry has Rounds %d, want %d", got[0].Rounds, maxEntries+19)
	}
}

func TestAMissingLogIsEmptyNotAnError(t *testing.T) {
	if got := Read(filepath.Join(t.TempDir(), "nothing.jsonl"), 10); len(got) != 0 {
		t.Errorf("a missing log returned %d runs", len(got))
	}
}

// One damaged line must not hide the runs recorded around it.
func TestABrokenLineIsSkipped(t *testing.T) {
	p := path(t)
	if err := Append(p, Run{Key: "acme/api#1", Outcome: OK}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json at all\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Append(p, Run{Key: "acme/api#2", Outcome: OK}); err != nil {
		t.Fatal(err)
	}
	got := Read(p, 0)
	if len(got) != 2 {
		t.Fatalf("read %d runs around a broken line, want 2", len(got))
	}
}

// Several runs can finish at the same moment, and an append rewrites the whole
// file, so without the lock they would overwrite each other's entries.
func TestConcurrentAppendsKeepEveryEntry(t *testing.T) {
	p := path(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Append(p, Run{Key: "acme/api#1", Outcome: OK, Rounds: i}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := Read(p, 0); len(got) != 20 {
		t.Errorf("20 concurrent appends left %d entries", len(got))
	}
}

func TestRunSplitsItsKey(t *testing.T) {
	r := Run{Key: "acme-org/some-service#2051"}
	if got := r.Name(); got != "some-service" {
		t.Errorf("Name = %q", got)
	}
	if got := r.Number(); got != 2051 {
		t.Errorf("Number = %d", got)
	}
	if got := r.URL(); !strings.HasSuffix(got, "/acme-org/some-service/pull/2051") {
		t.Errorf("URL = %q", got)
	}
	// A key that is not in the expected shape must not panic or invent a number.
	empty := Run{Key: "nonsense"}
	if got := empty.Number(); got != 0 {
		t.Errorf("Number of a malformed key = %d", got)
	}
}

func TestBranchRunUsesAStableKeyAndURL(t *testing.T) {
	r := Run{
		Key:    BranchKey("acme/api", "feature/crumb-tray"),
		Branch: "feature/crumb-tray",
	}
	if got := r.Key; got != "acme/api#branch:feature/crumb-tray" {
		t.Errorf("Key = %q", got)
	}
	if got := r.Name(); got != "api" {
		t.Errorf("Name = %q", got)
	}
	if got := r.Number(); got != 0 {
		t.Errorf("Number = %d", got)
	}
	if got := r.URL(); !strings.HasSuffix(got, "/acme/api/tree/feature/crumb-tray") {
		t.Errorf("URL = %q", got)
	}
}

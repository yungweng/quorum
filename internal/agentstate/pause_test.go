package agentstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWritePauseThenReadPause(t *testing.T) {
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	wrote, err := WritePause(dir, until)
	if err != nil {
		t.Fatalf("WritePause: %v", err)
	}
	if !wrote {
		t.Fatal("first pause did not report wrote")
	}
	got, ok := ReadPause(dir)
	if !ok || !got.Equal(until) {
		t.Fatalf("ReadPause = (%v, %v), want (%v, true)", got, ok, until)
	}
}

// Several reviews can hit the limit within moments of each other. Only the
// write that extends the deadline reports wrote, so exactly one notification
// goes out and a later reset time is never shortened.
func TestWritePauseKeepsTheLaterDeadline(t *testing.T) {
	dir := t.TempDir()
	later := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if _, err := WritePause(dir, later); err != nil {
		t.Fatal(err)
	}
	wrote, err := WritePause(dir, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("an earlier deadline replaced a later one")
	}
	if got, _ := ReadPause(dir); !got.Equal(later) {
		t.Fatalf("pause = %v, want the later %v", got, later)
	}
}

func TestReadPauseIgnoresExpiredMissingAndGarbage(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadPause(dir); ok {
		t.Fatal("a missing pause file read as paused")
	}
	if _, err := WritePause(dir, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadPause(dir); ok {
		t.Fatal("an expired pause read as active")
	}
	if err := os.WriteFile(filepath.Join(dir, pauseFile), []byte("not a time\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadPause(dir); ok {
		t.Fatal("garbage read as an active pause")
	}
}

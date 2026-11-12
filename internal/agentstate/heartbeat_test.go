package agentstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHeartbeatRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-poll")
	if _, _, ok := ReadHeartbeat(path); ok {
		t.Fatal("missing heartbeat read as present")
	}
	before := time.Now().Truncate(time.Second)
	WriteHeartbeat(path, 7)
	at, open, ok := ReadHeartbeat(path)
	if !ok || open != 7 || at.Before(before) {
		t.Fatalf("heartbeat = %v %d %v", at, open, ok)
	}
}

func TestHeartbeatWithoutCountReadsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-poll")
	if err := os.WriteFile(path, []byte("2026-01-02T03:04:05Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, open, ok := ReadHeartbeat(path); !ok || open != 0 {
		t.Fatalf("open = %d, ok = %v", open, ok)
	}
}

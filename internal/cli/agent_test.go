package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/paths"
)

// A missing plist is the not-installed case, a freshly rendered one is
// current, and any drift in what install would write must read as stale.
func TestPlistStale(t *testing.T) {
	dir := t.TempDir()
	a := &app{
		cfg: config.Config{PollInterval: 300},
		p:   paths.P{Plist: filepath.Join(dir, "test.plist"), StateDir: dir},
	}

	if a.plistStale() {
		t.Fatal("a missing plist reads as stale, but it means the agent is not installed")
	}

	current, err := a.renderPlist()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.p.Plist, current, 0o644); err != nil {
		t.Fatal(err)
	}
	if a.plistStale() {
		t.Fatal("a freshly rendered plist reads as stale")
	}

	a.cfg.PollInterval = 120
	if !a.plistStale() {
		t.Fatal("a changed poll interval does not read as stale")
	}
}

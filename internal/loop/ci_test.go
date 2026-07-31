package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/gh"
)

func TestCIWaitPublishesItsPhase(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	r := &run{
		p:      &Pipeline{GH: gh.New("true")},
		o:      Options{RepoRoot: root},
		ctx:    context.Background(),
		rep:    NopReporter{},
		pr:     gh.FullPR{Number: 7},
		root:   root,
		logDir: logDir,
		prog: Progress{
			Phase: PhaseFix,
			Since: before.Add(-time.Hour),
			CI:    CIRed,
		},
	}

	if err := r.ensureCIGreen(); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadProgress(root)
	if !ok {
		t.Fatal("CI progress was not published")
	}
	if got.Phase != PhaseCI || got.Since.Before(before) {
		t.Errorf("phase = %q since %v, want %q starting after %v", got.Phase, got.Since, PhaseCI, before)
	}
	if got.CI != CIGreen {
		t.Errorf("CI = %q, want %q", got.CI, CIGreen)
	}
}

func TestWatchCIRetriesTransientWatchFailure(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "gh")
	script := `#!/bin/sh
n=0
test ! -f "` + countFile + `" || n=$(cat "` + countFile + `")
n=$((n + 1))
printf '%s' "$n" > "` + countFile + `"
if test "$n" -eq 1; then
  echo 'net/http: TLS handshake timeout' >&2
  exit 1
fi
echo 'checks passed'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p: &Pipeline{GH: gh.New(bin)}, o: Options{RepoRoot: dir},
		ctx: context.Background(), logDir: dir, pr: gh.FullPR{Number: 42},
	}

	state, err := r.watchCI()
	if err != nil {
		t.Fatal(err)
	}
	if state != gh.ChecksPass {
		t.Fatalf("state = %v, want ChecksPass", state)
	}
	if calls, err := os.ReadFile(countFile); err != nil || string(calls) != "2" {
		t.Fatalf("watch calls = %q, err = %v", calls, err)
	}
}

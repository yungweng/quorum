package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRunDir(t *testing.T) {
	// The real header line, as pr-codex-review's row() prints it.
	dir, ok := parseRunDir("  Run dir    /Users/x/.cache/pr-codex-review/acme-api-pr-1-20260726-225935\n")
	if !ok || dir != "/Users/x/.cache/pr-codex-review/acme-api-pr-1-20260726-225935" {
		t.Errorf("got %q, ok=%v", dir, ok)
	}
	for _, line := range []string{
		"  Worktree  /Users/x/somewhere",
		"Run directory is not this line",
		"  Run dir",
		"  Run dir    relative/path",
		"",
	} {
		if _, ok := parseRunDir(line); ok {
			t.Errorf("wrongly parsed a run directory out of %q", line)
		}
	}
}

// The scanner must find the run directory in a stream that arrives in
// arbitrary chunks, which is how a pipe delivers it.
func TestRunDirScannerAcrossWrites(t *testing.T) {
	const want = "/Users/x/.cache/pr-codex-review/acme-api-pr-1-20260726-225935"
	output := "pr-codex-review v1.6.0\n" +
		"  PR        #1 something\n" +
		"  Run dir    " + want + "\n" +
		"  Reviewers 6\n"

	for _, chunk := range []int{1, 3, 17, len(output)} {
		var sink bytes.Buffer
		var seen string
		s := &runDirScanner{out: &sink, found: func(d string) { seen = d }}
		for i := 0; i < len(output); i += chunk {
			end := min(i+chunk, len(output))
			if _, err := s.Write([]byte(output[i:end])); err != nil {
				t.Fatal(err)
			}
		}
		s.flush()
		if s.dir != want {
			t.Errorf("chunk %d: dir = %q", chunk, s.dir)
		}
		if seen != want {
			t.Errorf("chunk %d: callback got %q", chunk, seen)
		}
		// Everything still has to reach the log untouched.
		if sink.String() != output {
			t.Errorf("chunk %d: log content changed", chunk)
		}
	}
}

func TestSafeName(t *testing.T) {
	if got := SafeName("moto-nrw/project-phoenix#2017"); got != "moto-nrw-project-phoenix-2017" {
		t.Errorf("got %q", got)
	}
}

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
	m.Release()
	if live := Live(dir); len(live) != 0 {
		t.Errorf("marker survived Release: %+v", live)
	}
}

func TestMarkerStaleIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	key := "acme/api#2"
	// pid 0 is never a live process here, so this stands in for a crashed run.
	path := filepath.Join(dir, SafeName(key))
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

func TestReadProgress(t *testing.T) {
	runDir := t.TempDir()
	if _, ok := ReadProgress(runDir, 6); ok {
		t.Error("progress reported for a run with no event log")
	}
	if _, ok := ReadProgress("", 6); ok {
		t.Error("progress reported for an empty run directory")
	}

	out := filepath.Join(runDir, "output")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// The event lines pr-codex-review appends: "ok idx elapsed rank" and
	// "fail idx exit elapsed".
	events := "ok 2 614 1\nok 6 634 2\nfail 4 124 700\nok 3 711 3\n"
	if err := os.WriteFile(filepath.Join(out, "events.log"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	p, ok := ReadProgress(runDir, 6)
	if !ok {
		t.Fatal("no progress read")
	}
	if p.Done != 3 || p.Failed != 1 || p.Requested != 6 {
		t.Errorf("progress = %+v", p)
	}
}

func TestReadFindings(t *testing.T) {
	runDir := t.TempDir()
	if _, err := readFindings(""); err == nil {
		t.Error("a missing run directory was accepted")
	}
	if _, err := readFindings(runDir); err == nil {
		t.Error("a missing findings file was accepted")
	}

	out := filepath.Join(runDir, "output")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"pr":2017,"blockers":0,"critical":4,"suggestions":2,"questions":1,
	          "reviewers_succeeded":6,"reviewers_requested":6,"posted":true,
	          "comment_url":"https://github.com/acme/api/pull/2017#issuecomment-1"}`
	if err := os.WriteFile(filepath.Join(out, "findings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := readFindings(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if f.Critical != 4 || f.Suggestions != 2 || f.Questions != 1 || !f.Posted {
		t.Errorf("findings = %+v", f)
	}
	if f.CommentURL == "" {
		t.Error("comment url missing")
	}
}

func TestSummaryLine(t *testing.T) {
	if got := summaryLine(Findings{}); got != "nothing found" {
		t.Errorf("empty findings rendered as %q", got)
	}
	if got := summaryLine(Findings{Critical: 4}); got != "0 blockers, 4 critical, 0 suggestions" {
		t.Errorf("got %q", got)
	}
	// A run that only raised questions still has something to report.
	if got := summaryLine(Findings{Questions: 2}); got == "nothing found" {
		t.Error("questions were reported as nothing found")
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

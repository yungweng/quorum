package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SessionsDir is where the Codex CLI records its rollout files, one per
// session, bucketed by local date.
func SessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".codex", "sessions")
}

// rolloutMeta is the first line of a rollout file. Verified against Codex CLI
// 0.145.0, which writes:
//
//	{"timestamp":"…","type":"session_meta","payload":{"session_id":"019f…",
//	 "cwd":"/path/to/worktree","originator":"codex_exec",…}}
//
// The shell version dug the id out of the file name with a UUID regex; taking
// it from the payload is the same value without the parse.
type rolloutMeta struct {
	Type    string `json:"type"`
	Payload struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	} `json:"payload"`
}

// FindSession returns the id of the Codex session whose working directory is
// worktree. Every run gets its own worktree path, and that uniqueness is the
// entire safety argument: concurrent Codex sessions started elsewhere on the
// machine cannot match, so resuming can never hijack one of them. Keep it that
// way if the run directory layout ever changes.
//
// Today's and yesterday's buckets are both searched, because a run that starts
// before midnight resumes after it.
func FindSession(worktree string) (string, error) {
	now := time.Now()
	root := SessionsDir()
	dirs := []string{
		filepath.Join(root, now.Format("2006/01/02")),
		filepath.Join(root, now.AddDate(0, 0, -1).Format("2006/01/02")),
	}

	for _, dir := range dirs {
		files, err := rolloutFilesByAge(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			meta, err := readRolloutMeta(f)
			if err != nil {
				continue
			}
			if meta.Payload.CWD == worktree && meta.Payload.SessionID != "" {
				return meta.Payload.SessionID, nil
			}
		}
	}
	return "", fmt.Errorf("no Codex session found for %s under %s", worktree, root)
}

// rolloutFilesByAge lists a day's rollout files, newest first, so the most
// recent session for a worktree wins.
func rolloutFilesByAge(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "rollout-*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, err
	}
	type entry struct {
		path string
		mod  time.Time
	}
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		entries = append(entries, entry{m, info.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.After(entries[j].mod) })

	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.path
	}
	return out, nil
}

// readRolloutMeta parses only the first line. The rest of a rollout file is the
// full conversation and can reach many megabytes, none of which is needed here.
func readRolloutMeta(path string) (rolloutMeta, error) {
	var meta rolloutMeta
	f, err := os.Open(path)
	if err != nil {
		return meta, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Base instructions are inlined in the meta line, so the default 64 KB
	// scanner buffer is not enough.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		return meta, fmt.Errorf("%s: empty", path)
	}
	if err := json.Unmarshal(sc.Bytes(), &meta); err != nil {
		return meta, err
	}
	if meta.Type != "session_meta" {
		return meta, fmt.Errorf("%s: first line is %q, not session_meta", path, meta.Type)
	}
	return meta, nil
}

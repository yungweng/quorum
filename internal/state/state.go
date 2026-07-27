// Package state stores what quorum knows about every pull request it has seen.
//
// Reviews run as separate processes and all write the same file, so every
// mutation takes an exclusive lock, re-reads, changes and writes atomically.
// The file keeps the shape the shell version wrote, and reads values it wrote
// as strings where numbers now live.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Status values a record can carry.
const (
	Pending  = "pending"  // request seen, waiting for a free slot
	Deferred = "deferred" // held back on purpose, Reason says why
	Running  = "running"
	OK       = "ok"
	Failed   = "failed"
	Skipped  = "skipped" // will not be reviewed, Reason says why
	GaveUp   = "gaveup"  // too many failed attempts
)

// keep bounds how many records survive a write, newest first.
const keep = 200

// Num is an int that also unmarshals from the JSON strings the shell version
// wrote, so an existing state file loads without a migration step.
type Num int

func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" || s == "-" {
		*n = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("not a number: %s", string(b))
	}
	*n = Num(v)
	return nil
}

func (n Num) MarshalJSON() ([]byte, error) { return json.Marshal(int(n)) }

// Record is everything known about one pull request.
type Record struct {
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
	Reason string `json:"reason,omitempty"`

	SHA string `json:"sha,omitempty"`
	// ReqAt is the timestamp of the review request that has been reviewed. A
	// newer request carries a newer timestamp, which is what makes a
	// re-request trigger a fresh review. The shell version wrote this field
	// with the same meaning, so an existing state file does not re-review
	// everything after the upgrade.
	ReqAt string `json:"req_at,omitempty"`
	// SeenReqAt is the newest request timestamp observed, reviewed or not. It
	// is the cached answer belonging to TimelineAt, and is empty both for "no
	// request found" and for "never looked", which is why TimelineAt exists.
	SeenReqAt string `json:"seen_req_at,omitempty"`
	// TimelineAt is the PR's updatedAt at the moment its timeline was last
	// read. While the pull request has not changed since, the timeline cannot
	// have gained a request, so it is not fetched again. It is deliberately
	// separate from anything written when a PR is merely skipped: "read the
	// timeline and found nothing" and "never read the timeline" must not look
	// the same.
	TimelineAt string `json:"timeline_at,omitempty"`
	// At is when something actually happened to this pull request: a review
	// started, finished, failed, or was decided against. Bookkeeping such as
	// caching a timeline lookup deliberately leaves it alone, otherwise merely
	// re-checking an old pull request would make its finished review look like
	// it had just come back.
	At string `json:"at,omitempty"`
	// StartedAt is when the current or last review run began.
	StartedAt string `json:"started_at,omitempty"`

	CommentURL  string `json:"comment_url,omitempty"`
	Blockers    Num    `json:"blockers,omitempty"`
	Critical    Num    `json:"critical,omitempty"`
	Suggestions Num    `json:"suggestions,omitempty"`
	Questions   Num    `json:"questions,omitempty"`

	Fails  Num    `json:"fails,omitempty"`
	RunLog string `json:"runlog,omitempty"`
	// RunDir is pr-codex-review's run directory. status reads its events.log to
	// show reviewer progress while a review is in flight.
	RunDir string `json:"run_dir,omitempty"`
}

// File is the on-disk document.
type File struct {
	Schema int               `json:"schema"`
	PRs    map[string]Record `json:"prs"`
}

// Entry pairs a record with its "owner/repo#number" key.
type Entry struct {
	Key string
	Record
}

// Repo returns the owner/repo half of the key.
func (e Entry) Repo() string {
	if i := strings.LastIndex(e.Key, "#"); i > 0 {
		return e.Key[:i]
	}
	return e.Key
}

// Number returns the pull request number, or 0 for a malformed key.
func (e Entry) Number() int {
	if i := strings.LastIndex(e.Key, "#"); i > 0 {
		n, _ := strconv.Atoi(e.Key[i+1:])
		return n
	}
	return 0
}

// Name returns just the repository name, which is what fits on a status line.
func (e Entry) Name() string {
	repo := e.Repo()
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// URL is the pull request page.
func (e Entry) URL() string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", e.Repo(), e.Number())
}

// Time parses At, returning the zero time when it is missing or malformed.
func (e Entry) Time() time.Time {
	return parseTime(e.At)
}

// Started parses StartedAt the same way.
func (e Entry) Started() time.Time {
	return parseTime(e.StartedAt)
}

// parseTime accepts both the RFC3339 written now and the local timestamp
// without a zone that the shell version wrote.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Now formats a timestamp the way records store it.
func Now() string { return time.Now().Format(time.RFC3339) }

// Mark records a change of state and stamps the time it happened. Every
// transition worth showing goes through here; anything that only updates a
// cache sets its fields directly and leaves At untouched.
func (r *Record) Mark(status, reason string) {
	r.Status = status
	r.Reason = reason
	r.At = Now()
}

// Read loads the state file. A missing file is an empty state, not an error.
func Read(path string) (File, error) {
	f := File{Schema: 1, PRs: map[string]Record{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if len(b) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	if f.PRs == nil {
		f.PRs = map[string]Record{}
	}
	return f, nil
}

// Entries returns every record, newest change first.
func (f File) Entries() []Entry {
	out := make([]Entry, 0, len(f.PRs))
	for k, r := range f.PRs {
		out = append(out, Entry{Key: k, Record: r})
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].Time(), out[j].Time()
		if ti.Equal(tj) {
			return out[i].Key < out[j].Key
		}
		return ti.After(tj)
	})
	return out
}

// Mutate applies fn to the record under key while holding the file lock, then
// writes the result atomically. fn receives the current record, or a zero one
// for a pull request that has not been seen before.
func Mutate(path, key string, fn func(*Record)) error {
	unlock, err := lock(path)
	if err != nil {
		return err
	}
	defer unlock()

	f, err := Read(path)
	if err != nil {
		return err
	}
	rec := f.PRs[key]
	fn(&rec)
	f.PRs[key] = rec
	return write(path, f)
}

// write trims the document to the newest records and replaces the file in one
// rename, so a reader never sees a half-written state.
func write(path string, f File) error {
	if f.Schema == 0 {
		f.Schema = 1
	}
	if len(f.PRs) > keep {
		entries := f.Entries()
		trimmed := make(map[string]Record, keep)
		for _, e := range entries[:keep] {
			trimmed[e.Key] = e.Record
		}
		f.PRs = trimmed
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// lock takes an exclusive advisory lock for the state file and returns the
// release function. It blocks until the lock is available; every holder only
// reads, changes and writes, so the wait is bounded by one small file write.
func lock(statePath string) (func(), error) {
	lockPath := statePath + ".lock"
	// The shell version used a directory of this name as its lock. Left behind
	// by a killed poll, it would make opening the lock file fail forever.
	if fi, err := os.Stat(lockPath); err == nil && fi.IsDir() {
		os.RemoveAll(lockPath)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, errors.New("could not lock " + lockPath + ": " + err.Error())
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

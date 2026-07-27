package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

// recentCount is how many finished reviews the dashboard shows.
const recentCount = 8

func (a *app) cmdStatus(args []string) int {
	_ = args
	a.dashboard(a.out, nil)
	return 0
}

// dashboard renders everything quorum knows in one screen: what is running, what
// is waiting, what came back, and whether the machine is in a state to do more.
//
// ends says how a pull request finished on GitHub, keyed the same way as the
// state file and holding gh.StateMerged and friends. It is optional: only watch
// looks that up, because it is the only caller that can afford to ask GitHub in
// the background. dashboard returns the keys it drew, which is how watch knows
// what is worth looking up next time.
func (a *app) dashboard(w *ui.Writer, ends map[string]string) []string {
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		w.Printf("%s\n", w.Red("state file unreadable: "+err.Error()))
	}
	live := map[string]runner.Marker{}
	for _, m := range runner.Live(a.p.RunningDir) {
		live[m.Key] = m
	}

	a.header(w)

	var running, queued, recent []state.Entry
	for _, e := range file.Entries() {
		switch e.Status {
		case state.Running:
			running = append(running, e)
		case state.Pending, state.Deferred:
			queued = append(queued, e)
		default:
			recent = append(recent, e)
		}
	}
	// Oldest request first in the queue, matching the order they will start in.
	sort.SliceStable(queued, func(i, j int) bool { return queued[i].SeenReqAt < queued[j].SeenReqAt })
	if len(recent) > recentCount {
		recent = recent[:recentCount]
	}

	a.sectionRunning(w, running, live, ends)
	a.sectionQueued(w, queued, ends)
	a.sectionRecent(w, recent, ends)
	a.sectionSystem(w)

	shown := make([]string, 0, len(running)+len(queued)+len(recent))
	for _, group := range [][]state.Entry{running, queued, recent} {
		for _, e := range group {
			shown = append(shown, e.Key)
		}
	}
	return shown
}

func (a *app) header(w *ui.Writer) {
	left := "quorum " + Version
	right := a.agentLine()
	gap := w.Width - len(left) - len(right) - 1
	if gap < 2 {
		w.Printf("%s\n%s\n", w.Bold(left), w.Dim(right))
		return
	}
	w.Printf("%s%s%s\n", w.Bold(left), strings.Repeat(" ", gap), w.Dim(right))
	if a.configErr != nil {
		w.Printf("%s\n", w.Yellow("config: "+a.configErr.Error()))
	}
}

// agentLine describes the launchd agent and when it last actually did work.
func (a *app) agentLine() string {
	if !a.agentLoaded() {
		return "agent not installed, run: quorum install"
	}
	every := fmt.Sprintf("every %s", ui.Duration(time.Duration(a.cfg.PollInterval)*time.Second))
	at, _, ok := a.lastPoll()
	if !ok {
		return "agent loaded, " + every + ", no poll yet"
	}
	return fmt.Sprintf("agent loaded, %s, last poll %s", every, ui.Ago(at))
}

// A section that disappears when it is empty cannot be told apart from a
// section that does not exist, which is the wrong answer to "is anything being
// reviewed right now". Both stages of the pipeline therefore keep their
// heading and say so in words.
func (a *app) sectionRunning(w *ui.Writer, running []state.Entry, live map[string]runner.Marker, ends map[string]string) {
	w.Section("running", len(running), a.cfg.MaxConcurrent)
	if len(running) == 0 {
		w.Printf("  %s\n", w.Dim("nothing under review right now"))
		return
	}
	for _, e := range running {
		_, alive := live[e.Key]
		a.prLine(w, w.Cyan("●"), e, ends[e.Key])
		detail := ""
		if !alive {
			detail = w.Red("process gone, will be retried")
		} else {
			since := ""
			if t := e.Started(); !t.IsZero() {
				since = ui.Duration(time.Since(t))
			}
			progress := "starting up"
			if p, ok := runner.ReadProgress(e.RunDir, a.cfg.Reviewers); ok {
				progress = fmt.Sprintf("%d/%d reviewers done", p.Done, p.Requested)
				if p.Failed > 0 {
					progress += fmt.Sprintf(", %d failed", p.Failed)
				}
				if p.Done+p.Failed >= p.Requested {
					progress = "aggregating"
				}
			}
			detail = w.Dim(strings.TrimPrefix(since+", ", ", ") + progress)
		}
		w.Printf("    %s\n", detail)
	}
}

func (a *app) sectionQueued(w *ui.Writer, queued []state.Entry, ends map[string]string) {
	w.Section("queued", len(queued), 0)
	if len(queued) == 0 {
		w.Printf("  %s\n", w.Dim("nothing waiting"))
		return
	}
	for _, e := range queued {
		a.prLine(w, w.Dim("○"), e, ends[e.Key])
		reason := e.Reason
		if reason == "" {
			reason = "waiting"
		}
		if e.Status == state.Deferred {
			reason = "held back: " + reason
		}
		w.Printf("    %s\n", w.Dim(reason))
	}
}

func (a *app) sectionRecent(w *ui.Writer, recent []state.Entry, ends map[string]string) {
	if len(recent) == 0 {
		return
	}
	w.Section("recent", 0, 0)
	for _, e := range recent {
		var mark, detail string
		switch e.Status {
		case state.OK:
			mark = w.Green("✓")
			detail = findingsText(w, e)
		case state.Failed:
			mark = w.Red("✗")
			detail = w.Red(fmt.Sprintf("failed after %d attempt(s)", e.Fails))
			if e.Reason != "" {
				detail += w.Dim(", " + ui.Truncate(e.Reason, 40))
			}
		case state.GaveUp:
			mark = w.Red("✗")
			detail = w.Red("gave up") + w.Dim(", retry: quorum run "+e.Key)
		default:
			mark = w.Dim("–")
			detail = w.Dim("skipped: " + e.Reason)
		}
		// One line each: what came back matters more here than the title.
		w.Printf("  %s %s%s %s %s%s\n",
			mark,
			prLabel(w, e, ends[e.Key]),
			labelPad(e),
			w.Dim(ui.Pad(ui.Ago(e.Time()), 10)),
			detail,
			endSuffix(w, ends[e.Key]))
	}
}

// findingsText is the one line summary of a finished review, with the posted
// comment behind a link.
func findingsText(w *ui.Writer, e state.Entry) string {
	counts := fmt.Sprintf("%d blockers, %d critical, %d suggestions", e.Blockers, e.Critical, e.Suggestions)
	if e.Blockers == 0 && e.Critical == 0 && e.Suggestions == 0 && e.Questions == 0 {
		counts = "nothing found"
	}
	if e.Blockers > 0 {
		counts = w.Red(counts)
	} else if e.Critical > 0 {
		counts = w.Yellow(counts)
	}
	if e.CommentURL != "" {
		return counts + "  " + w.Link(w.Blue("comment ↗"), e.CommentURL)
	}
	return counts + "  " + w.Dim("not posted")
}

// labelWidth keeps the repository and number column aligned across sections.
const labelWidth = 26

func labelOf(e state.Entry) string {
	return fmt.Sprintf("%s #%d", e.Name(), e.Number())
}

// prLabel renders the "project-phoenix #2017" column, linked to the pull
// request and crossed out once that pull request has been merged: the work
// landed, so the line is history rather than something to look at.
func prLabel(w *ui.Writer, e state.Entry, end string) string {
	styled := w.Bold(labelOf(e))
	if end == gh.StateMerged {
		styled = w.Strike(styled)
	}
	return w.Link(styled, e.URL())
}

// labelPad is the spacing that keeps the column after the label aligned. It
// stays outside prLabel so a strikethrough ends with the text instead of
// trailing off across empty space, and because padding inside the styling
// would be counted in escape bytes rather than in visible cells.
func labelPad(e state.Entry) string {
	if n := labelWidth - utf8.RuneCountInString(labelOf(e)); n > 0 {
		return strings.Repeat(" ", n)
	}
	return ""
}

// endWord names how a pull request ended, in plain text.
//
// The strikethrough already says "merged" to anyone whose terminal draws SGR 9,
// but plain output and older terminals drop that attribute without a trace, so
// the word is what carries the meaning and the styling only makes it faster to
// read. It is returned unstyled because callers have to measure it: it shares a
// line with a title, and a line wider than the terminal wraps.
func endWord(end string) string {
	switch end {
	case gh.StateMerged:
		return "merged"
	case gh.StateClosed:
		// Closed without merging is not a success, so it is never struck out.
		return "closed unmerged"
	}
	return ""
}

func endSuffix(w *ui.Writer, end string) string {
	if word := endWord(end); word != "" {
		return w.Dim("  " + word)
	}
	return ""
}

// prLine prints "  ● project-phoenix #2017  the title", with the repository and
// number linking to the pull request.
func (a *app) prLine(w *ui.Writer, mark string, e state.Entry, end string) {
	if e.Title == "" {
		// Records written before titles were stored have nothing to show, and
		// padding to an empty column just leaves trailing whitespace.
		w.Printf("  %s %s%s\n", mark, prLabel(w, e, end), endSuffix(w, end))
		return
	}
	titleRoom := max(w.Width-labelWidth-8-len(endWord(end)), 12)
	w.Printf("  %s %s%s  %s%s\n",
		mark,
		prLabel(w, e, end),
		labelPad(e),
		ui.Truncate(e.Title, titleRoom),
		endSuffix(w, end))
}

func (a *app) sectionSystem(w *ui.Writer) {
	w.Section("system", 0, 0)

	scope := "every repo that asks you"
	if len(a.cfg.Orgs) > 0 || len(a.cfg.Repos) > 0 {
		scope = strings.Join(append(append([]string{}, a.cfg.Orgs...), a.cfg.Repos...), " ")
	}
	w.Printf("  %s %s\n", w.Dim(ui.Pad("scope", 10)), scope)

	budget := fmt.Sprintf("%d reviews at a time, %d reviewers each, so up to %d Codex processes",
		a.cfg.MaxConcurrent, a.cfg.Reviewers, a.cfg.Codex())
	w.Printf("  %s %s\n", w.Dim(ui.Pad("budget", 10)), budget)

	if load, ok := runner.LoadAvg1(); ok {
		text := fmt.Sprintf("%.1f", load)
		if a.cfg.LoadLimit > 0 {
			text += fmt.Sprintf(" of %.0f before reviews are held back", a.cfg.LoadLimit)
			if load > a.cfg.LoadLimit {
				text = w.Yellow(text)
			}
		}
		w.Printf("  %s %s\n", w.Dim(ui.Pad("load", 10)), text)
	}

	size := a.cacheSize()
	cache := ui.Bytes(size)
	if limit := a.budgetBytes(); limit > 0 {
		cache += " of " + ui.Bytes(limit)
		if size > limit {
			cache = w.Yellow(cache) + w.Dim("  run: quorum gc")
		}
	}
	w.Printf("  %s %s\n", w.Dim(ui.Pad("cache", 10)), cache)
	fmt.Fprintln(w.Out)
}

// lastPoll reads the heartbeat the poll writes when it finishes.
func (a *app) lastPoll() (time.Time, int, bool) {
	b, err := os.ReadFile(filepath.Join(a.p.StateDir, "last-poll"))
	if err != nil {
		return time.Time{}, 0, false
	}
	parts := strings.Fields(string(b))
	if len(parts) == 0 {
		return time.Time{}, 0, false
	}
	t, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return time.Time{}, 0, false
	}
	open := 0
	if len(parts) > 1 {
		open, _ = strconv.Atoi(parts[1])
	}
	return t, open, true
}

// dirSize adds up a directory tree, ignoring anything it cannot read.
func dirSize(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

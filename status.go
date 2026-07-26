package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/prbot/internal/runner"
	"github.com/yungweng/prbot/internal/state"
	"github.com/yungweng/prbot/internal/ui"
)

// recentCount is how many finished reviews the dashboard shows.
const recentCount = 8

func (a *app) cmdStatus(args []string) int {
	_ = args
	a.dashboard(a.out)
	return 0
}

// dashboard renders everything prbot knows in one screen: what is running, what
// is waiting, what came back, and whether the machine is in a state to do more.
func (a *app) dashboard(w *ui.Writer) {
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

	a.sectionRunning(w, running, live)
	a.sectionQueued(w, queued)
	a.sectionRecent(w, recent)
	a.sectionSystem(w)
}

func (a *app) header(w *ui.Writer) {
	left := "prbot " + Version
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
		return "agent not installed, run: prbot install"
	}
	every := fmt.Sprintf("every %s", ui.Duration(time.Duration(a.cfg.PollInterval)*time.Second))
	at, _, ok := a.lastPoll()
	if !ok {
		return "agent loaded, " + every + ", no poll yet"
	}
	return fmt.Sprintf("agent loaded, %s, last poll %s", every, ui.Ago(at))
}

func (a *app) sectionRunning(w *ui.Writer, running []state.Entry, live map[string]runner.Marker) {
	if len(running) == 0 {
		return
	}
	w.Section("running", len(running), a.cfg.MaxConcurrent)
	for _, e := range running {
		_, alive := live[e.Key]
		a.prLine(w, w.Cyan("●"), e)
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

func (a *app) sectionQueued(w *ui.Writer, queued []state.Entry) {
	if len(queued) == 0 {
		return
	}
	w.Section("queued", len(queued), 0)
	for _, e := range queued {
		a.prLine(w, w.Dim("○"), e)
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

func (a *app) sectionRecent(w *ui.Writer, recent []state.Entry) {
	if len(recent) == 0 {
		return
	}
	if len(recent) > recentCount {
		recent = recent[:recentCount]
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
			detail = w.Red("gave up") + w.Dim(", retry: prbot run "+e.Key)
		default:
			mark = w.Dim("–")
			detail = w.Dim("skipped: " + e.Reason)
		}
		// One line each: what came back matters more here than the title.
		w.Printf("  %s %s %s %s\n",
			mark,
			w.Link(w.Bold(ui.Pad(labelOf(e), labelWidth)), e.URL()),
			w.Dim(ui.Pad(ui.Ago(e.Time()), 10)),
			detail)
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

// prLine prints "  ● project-phoenix #2017  the title", with the repository and
// number linking to the pull request.
func (a *app) prLine(w *ui.Writer, mark string, e state.Entry) {
	if e.Title == "" {
		// Records written before titles were stored have nothing to show, and
		// padding to an empty column just leaves trailing whitespace.
		w.Printf("  %s %s\n", mark, w.Link(w.Bold(labelOf(e)), e.URL()))
		return
	}
	titleRoom := max(w.Width-labelWidth-8, 12)
	w.Printf("  %s %s  %s\n",
		mark,
		w.Link(w.Bold(ui.Pad(labelOf(e), labelWidth)), e.URL()),
		ui.Truncate(e.Title, titleRoom))
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

	size := dirSize(a.p.ReviewCache)
	cache := ui.Bytes(size)
	if a.cfg.CacheBudgetGB > 0 {
		limit := int64(a.cfg.CacheBudgetGB * 1024 * 1024 * 1024)
		cache += " of " + ui.Bytes(limit)
		if size > limit {
			cache = w.Yellow(cache) + w.Dim("  run: prbot gc")
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

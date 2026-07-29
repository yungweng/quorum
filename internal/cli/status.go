package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

// recentCount is how many finished reviews the dashboard shows.
const recentCount = 8

const agentTTL = 30 * time.Second

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
	liveMarkers := runner.Live(a.p.RunningDir)
	for _, m := range liveMarkers {
		live[m.Key] = m
	}

	// Fix loops are found in the run cache rather than in the state file. One
	// the agent started is in both, and one you started in a terminal is only
	// here. An agent babysit keeps its scheduler slot; a terminal babysit never
	// takes one and never records anything.
	babysits := loop.LiveRuns(a.p.BabysitRuns)
	babysitPID := map[int]bool{}
	for _, p := range babysits {
		babysitPID[p.PID] = true
	}

	a.header(w)

	var running, queued, recent []state.Entry
	seen := map[string]bool{}
	for _, e := range file.Entries() {
		seen[e.Key] = true
		if m, ok := live[e.Key]; ok && !babysitPID[m.PID] && e.Status != state.Running {
			// A direct run claims its slot before it updates an existing state
			// record. While that preparation runs, the live claim is the more
			// current source of truth.
			running = append(running, e)
			continue
		}
		switch e.Status {
		case state.Running:
			// A fix loop the agent started has a record here as well, and its
			// own section says far more about it. The marker names the very
			// process the pipeline runs in, so matching on the pid removes
			// that record without also hiding an unrelated review of a pull
			// request somebody happens to be babysitting.
			if m, ok := live[e.Key]; ok && babysitPID[m.PID] {
				continue
			}
			running = append(running, e)
		case state.Pending, state.Deferred:
			queued = append(queued, e)
		default:
			recent = append(recent, e)
		}
	}
	for _, m := range liveMarkers {
		if !seen[m.Key] && !babysitPID[m.PID] {
			// A direct run also claims its slot before it creates a state
			// record. Keep it visible during PR lookup and clone preparation.
			running = append(running, state.Entry{Key: m.Key})
		}
	}
	// Oldest request first in the queue, matching the order they will start in.
	sort.SliceStable(queued, func(i, j int) bool { return queued[i].SeenReqAt < queued[j].SeenReqAt })
	if len(recent) > recentCount {
		recent = recent[:recentCount]
	}

	a.sectionReviewing(w, running, len(live), live, ends)
	a.sectionBabysitting(w, babysits, ends)
	a.sectionQueued(w, queued, ends)
	a.sectionRecent(w, recent, ends)
	a.sectionSystem(w)

	shown := make([]string, 0, len(running)+len(queued)+len(recent)+len(babysits))
	shownSet := make(map[string]bool, cap(shown))
	for _, group := range [][]state.Entry{running, queued, recent} {
		for _, e := range group {
			shown = append(shown, e.Key)
			shownSet[e.Key] = true
		}
	}
	for _, p := range babysits {
		if key := p.Key(); !shownSet[key] {
			shown = append(shown, key)
			shownSet[key] = true
		}
	}
	return shown
}

func (a *app) header(w *ui.Writer) {
	left := "quorum " + a.version
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
	if time.Since(a.agentAt) > agentTTL {
		a.agentOn, a.agentAt = a.agentLoaded(), time.Now()
	}
	if !a.agentOn {
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
// reviewed right now". Every stage of the pipeline therefore keeps its heading
// and says so in words.
func (a *app) sectionReviewing(w *ui.Writer, running []state.Entry, slotsUsed int, live map[string]runner.Marker, ends map[string]string) {
	w.Section("reviewing", slotsUsed, a.cfg.MaxConcurrent)
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

// sectionBabysitting shows the fix loops in flight, whichever way they were
// started. It carries no slot total because babysit has no independent
// concurrency budget. An agent-started loop's scheduler slot is reflected in
// the reviewing total instead.
func (a *app) sectionBabysitting(w *ui.Writer, runs []loop.Progress, ends map[string]string) {
	w.Section("babysitting", len(runs), 0)
	if len(runs) == 0 {
		w.Printf("  %s\n", w.Dim("no fix loop running"))
		return
	}
	now := time.Now()
	for _, p := range runs {
		e := state.Entry{Key: p.Key(), Record: state.Record{Title: p.Title}}
		a.prLine(w, w.Magenta("●"), e, ends[p.Key()])
		w.Printf("    %s\n", babysitTrack(w, p, now))
	}
}

// babysitTrack is the line under a fix loop: where it is in its loop, what it
// is doing now, and for how long.
//
// A run can sit in one phase for an hour, so the question the line has to
// answer is not "is it running" but "is it getting anywhere". Segments are
// added in that order of usefulness and stop at the terminal's width: a line
// that wraps pushes the rest of the frame off the bottom of the screen, which
// costs more than the commit count is worth.
func babysitTrack(w *ui.Writer, p loop.Progress, now time.Time) string {
	// Indent and separator have to match what the caller prints, or the budget
	// is measured against the wrong line.
	const indent = 4
	const gap = "   "

	var plain, styled []string
	add := func(text, style string) {
		plain = append(plain, text)
		styled = append(styled, style)
	}

	if !p.StartedAt.IsZero() {
		add(ui.Duration(now.Sub(p.StartedAt)), w.Dim(ui.Duration(now.Sub(p.StartedAt))))
	}
	if p.MaxIter > 0 {
		round := fmt.Sprintf("round %d/%d", max(p.Round, 1), p.MaxIter)
		add(round, w.Dim(round))
	}
	if text, style := ciSegment(w, p); text != "" {
		add(text, style)
	}
	if text, style := reviewSegment(w, p); text != "" {
		add(text, style)
	}
	if text, style := phaseSegment(w, p, now); text != "" {
		add(text, style)
	}
	if p.Commits > 0 {
		commits := fmt.Sprintf("%d commits", p.Commits)
		if p.Commits == 1 {
			commits = "1 commit"
		}
		add(commits, w.Dim(commits))
	}
	if p.RunDir != "" {
		add("log ↗", w.Link(w.Blue("log ↗"), "file://"+p.LogDir()))
	}

	room, used := w.Width-indent, 0
	var out []string
	for i, text := range plain {
		need := utf8.RuneCountInString(text)
		if len(out) > 0 {
			need += len(gap)
		}
		if used+need > room {
			break
		}
		used += need
		out = append(out, styled[i])
	}
	return strings.Join(out, gap)
}

// ciSegment reports the last check state. While the pipeline is still waiting
// for the first answer it says nothing: the phase segment already says that,
// and printing "CI unknown" next to "waiting for CI" is noise.
func ciSegment(w *ui.Writer, p loop.Progress) (string, string) {
	switch p.CI {
	case loop.CIGreen:
		return "CI ✓", w.Green("CI ✓")
	case loop.CIRed:
		text := fmt.Sprintf("CI ✗ fix %d/%d", p.CIFix, p.MaxCIFixes)
		return text, w.Red(text)
	case loop.CINone:
		return "CI none", w.Dim("CI none")
	}
	return "", ""
}

// reviewSegment reports the newest round result. Only Blockers and Critical
// appear: they are the two that keep the loop going, and a line that also
// carried suggestions would suggest those hold it up too.
func reviewSegment(w *ui.Writer, p loop.Progress) (string, string) {
	if !p.Reviewed {
		return "", ""
	}
	if p.Blockers+p.Critical == 0 {
		return "review ✓", w.Green("review ✓")
	}
	text := fmt.Sprintf("review %dB %dC", p.Blockers, p.Critical)
	if p.Blockers > 0 {
		return text, w.Red(text)
	}
	return text, w.Yellow(text)
}

// phaseSegment says what the run is doing now and since when.
func phaseSegment(w *ui.Writer, p loop.Progress, now time.Time) (string, string) {
	var what string
	switch p.Phase {
	case loop.PhaseStarting:
		what = "preparing"
	case loop.PhaseCI:
		what = "waiting for CI"
	case loop.PhaseReview:
		what = "reviewing"
	case loop.PhaseFix:
		what = "fixing"
	case loop.PhaseCIFix:
		what = fmt.Sprintf("CI fix %d", p.CIFix)
	default:
		return "", ""
	}
	if !p.Since.IsZero() {
		what += " ● " + ui.Duration(now.Sub(p.Since))
	}
	return what, w.Cyan(what)
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

// prLabel renders the "toaster-api #2017" column, linked to the pull
// request and crossed out once that pull request has been merged: the work
// landed, so the line is history rather than something to look at.
func prLabel(w *ui.Writer, e state.Entry, end string) string {
	return styledPRLabel(w, e, labelOf(e), end)
}

func styledPRLabel(w *ui.Writer, e state.Entry, label, end string) string {
	styled := w.Bold(label)
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

func truncateLabel(e state.Entry, width int) string {
	label := labelOf(e)
	if utf8.RuneCountInString(label) <= width {
		return label
	}
	suffix := fmt.Sprintf(" #%d", e.Number())
	if suffixWidth := utf8.RuneCountInString(suffix); suffixWidth < width {
		return ui.Truncate(e.Name(), width-suffixWidth) + suffix
	}
	return ui.Truncate(label, width)
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

// prLine prints "  ● toaster-api #2017  the title", with the repository and
// number linking to the pull request.
func (a *app) prLine(w *ui.Writer, mark string, e state.Entry, end string) {
	endWidth := 0
	if word := endWord(end); word != "" {
		endWidth = 2 + utf8.RuneCountInString(word)
	}
	if e.Title == "" {
		// Records written before titles were stored have nothing to show, and
		// padding to an empty column just leaves trailing whitespace.
		label := truncateLabel(e, max(w.Width-4-endWidth, 1))
		w.Printf("  %s %s%s\n", mark, styledPRLabel(w, e, label, end), endSuffix(w, end))
		return
	}
	available := max(w.Width-6-endWidth, 1)
	labelRoom := max(labelWidth, utf8.RuneCountInString(labelOf(e)))
	titleRoom := available - labelRoom
	if titleRoom < 12 {
		labelRoom = max(available-12, 1)
		titleRoom = max(available-labelRoom, 0)
	}
	label := truncateLabel(e, labelRoom)
	w.Printf("  %s %s%s  %s%s\n",
		mark,
		styledPRLabel(w, e, label, end),
		strings.Repeat(" ", max(labelRoom-utf8.RuneCountInString(label), 0)),
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

	cache := "no budget set"
	if limit := a.budgetBytes(); limit > 0 {
		cache = ui.Bytes(limit) + " budget"
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

// endStates caches how the pull requests on screen ended, so the redraw never
// has to wait for GitHub.
type endStates struct {
	mu     sync.Mutex
	state  map[string]string
	keys   []string
	change chan struct{}
}

func newEndStates() *endStates {
	return &endStates{state: map[string]string{}, change: make(chan struct{}, 1)}
}

// snapshot is what the dashboard reads: a copy, so rendering never holds the
// lock the refresher needs.
func (e *endStates) snapshot() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return maps.Clone(e.state)
}

// want records which pull requests are currently visible. A changed set wakes
// the refresher, so a pull request that appears is looked up straight away
// instead of waiting out the interval.
func (e *endStates) want(keys []string) {
	e.mu.Lock()
	same := slices.Equal(e.keys, keys)
	e.keys = keys
	e.mu.Unlock()
	if same {
		return
	}
	select {
	case e.change <- struct{}{}:
	default: // a wake-up is already pending, one is enough
	}
}

func (e *endStates) wanted() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.keys)
}

func (e *endStates) store(states map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = states
}

// trackEnds keeps the merge state of the visible pull requests roughly current.
//
// It runs beside the redraw and never inside it. The dashboard repaints every
// few seconds, and asking GitHub at that rate would make the screen wait on the
// network and earn a rate limit for a fact that only changes when somebody
// presses a button. One batched query covers every visible pull request.
func (a *app) trackEnds(ctx context.Context, ghBin string, e *endStates) {
	client := gh.New(ghBin)
	// No retries and a short deadline: this is decoration, and the next pass is
	// thirty seconds away, which is sooner than a retried call would answer.
	// Logging is off as well, because the logger echoes to the screen this is
	// drawing on.
	client.Attempts = 1
	client.Timeout = 20 * time.Second
	client.Log = nil

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.change:
		case <-time.After(30 * time.Second):
		}
		keys := e.wanted()
		if len(keys) == 0 {
			continue
		}
		states, err := client.PRStates(ctx, keys)
		if err != nil {
			// Nothing on the screen depends on this, so a failed lookup keeps
			// the previous answer rather than blanking what it already knows.
			continue
		}
		e.store(states)
	}
}

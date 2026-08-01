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

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

const agentTTL = 30 * time.Second

func (a *app) cmdStatus(args []string) int {
	_ = args
	a.dashboard(a.out, a.endsOnce())
	return 0
}

// endsOnce asks GitHub once how the recently reviewed pull requests ended.
//
// watch has a tracker for this and status does not, but status is the command
// that most needs the answer: without it the open section has no way to tell a
// pull request that is still waiting for someone from one that was merged an
// hour ago, and would list both. One batched query covers every candidate. It
// is decoration on a command that has always been instant, so it gets one
// attempt and a short deadline. A machine with no network uses the last known
// states; uncached pull requests are labelled as still being checked.
func (a *app) endsOnce() map[string]string {
	cached := readEndStateCache(a.p.PRStatesFile)
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		return cached
	}
	runs := history.Read(a.p.HistoryFile, 0)
	keys := recentReviewedPRKeys(reviewedPRs(file, runs), time.Now())
	if len(keys) == 0 {
		return cached
	}
	t, err := a.findTools()
	if err != nil {
		return cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := gh.New(t.GH)
	client.Attempts = 1
	client.Timeout = 5 * time.Second
	client.Log = nil
	states, err := client.PRStates(ctx, keys)
	if err != nil {
		return cached
	}
	_ = writeEndStateCache(a.p.PRStatesFile, states)
	return states
}

func recentReviewedPRKeys(reviewed []state.Entry, now time.Time) []string {
	cutoff := now.Add(-openWindow)
	var keys []string
	for _, e := range reviewed {
		if t := e.Time(); t.IsZero() || t.Before(cutoff) {
			continue
		}
		keys = append(keys, e.Key)
	}
	return keys
}

// dashboard renders everything quorum knows in one screen: what is running, what
// is waiting, what came back, and whether the machine is in a state to do more.
//
// ends says how a pull request finished on GitHub, keyed the same way as the
// state file and holding gh.StateMerged and friends. It is optional: status
// asks once before drawing, while watch refreshes it in the background.
// dashboard returns the keys watch must look up next. That includes every
// recent review which could belong under OPEN, even when a cached terminal
// state keeps it off screen, plus the pull requests drawn elsewhere.
func (a *app) dashboard(w *ui.Writer, ends map[string]string) []string {
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		w.Printf("%s\n", w.Red("state file unreadable: "+err.Error()))
	}
	historyRuns := history.Read(a.p.HistoryFile, 0)
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
	manualReviews := review.LiveRuns(a.p.ManualDir)
	manualKeys := make(map[string]bool, len(manualReviews))
	for _, run := range manualReviews {
		manualKeys[run.Key()] = true
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
		if manualKeys[e.Key] && e.Status != state.Running {
			// A manual review supersedes an older result or queue entry while
			// it runs. A concurrent agent review remains visible as its own
			// line so the two sources cannot be confused.
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

	a.statusbar(w, len(live), len(babysits), len(queued))

	// What is in flight is drawn first but printed second: the open section has
	// to leave out whatever the active section is about to show, or a pull
	// request being reviewed right now appears twice, once as a live run and
	// once as the result of the run before it.
	busy := make(map[string]bool, len(running)+len(queued)+len(manualReviews)+len(babysits))
	for _, group := range [][]state.Entry{running, queued} {
		for _, e := range group {
			busy[e.Key] = true
		}
	}
	for _, run := range manualReviews {
		busy[run.Key()] = true
	}
	for _, p := range babysits {
		busy[p.Key()] = true
	}
	reviewed := reviewedPRs(file, historyRuns)
	open, checking := openPRs(reviewed, busy, ends)

	// What is running gets as much room as it needs and no more, and the log of
	// finished runs gets the rest.
	//
	// The four fixed sections this replaced spent ten lines saying "nothing"
	// four times over, which is what an idle machine looks like almost all of
	// the time, while the runs that had actually happened were squeezed in
	// underneath. The counts that used to justify those headings are on the
	// status bar, so nothing is lost by collapsing them.
	limit := a.cfg.History
	if limit <= 0 {
		limit = config.Default().History
	}
	if len(recent) > limit {
		recent = recent[:limit]
	}
	groups := groupHistory(historyRuns)
	if len(groups) > limit {
		groups = groups[:limit]
	}
	if len(groups) > 0 {
		recent = nil
	}

	g := measure(w, open, running, manualReviews, babysits, queued, recent, groups)
	a.sectionOpen(w, g, open, checking, ends)
	a.sectionActive(w, g, running, manualReviews, babysits, queued, live, ends)
	past := a.sectionHistory(w, g, recent, groups, ends)
	a.footer(w)

	tracked := make([]string, 0, len(reviewed)+len(running)+len(manualReviews)+len(queued)+len(past)+len(babysits))
	trackedSet := make(map[string]bool, cap(tracked))
	for _, key := range recentReviewedPRKeys(reviewed, time.Now()) {
		tracked = append(tracked, key)
		trackedSet[key] = true
	}
	for _, group := range [][]state.Entry{open, running, queued} {
		for _, e := range group {
			if trackedSet[e.Key] {
				continue
			}
			tracked = append(tracked, e.Key)
			trackedSet[e.Key] = true
		}
	}
	for _, key := range past {
		if !trackedSet[key] {
			tracked = append(tracked, key)
			trackedSet[key] = true
		}
	}
	for _, run := range manualReviews {
		key := run.Key()
		if !trackedSet[key] {
			tracked = append(tracked, key)
			trackedSet[key] = true
		}
	}
	for _, p := range babysits {
		if key := p.Key(); !trackedSet[key] {
			tracked = append(tracked, key)
			trackedSet[key] = true
		}
	}
	return tracked
}

// measure takes the width of the shared columns from every row the frame is
// about to draw, before any of it is printed.
//
// Which rows are on screen is what decides how wide the repository column has
// to be, and that is not knowable while the first line is being written, which
// is why this runs over all of them first. Every row that carries the columns
// has to be counted here, runs on a branch included: what is measured is what
// the columns are cut to, so a row left out would be cut to a width taken from
// rows it has nothing to do with, and a frame holding nothing else would leave
// it nothing at all.
func measure(
	w *ui.Writer,
	open, running []state.Entry,
	manual []review.LiveRun,
	babysits []loop.Progress,
	queued, recent []state.Entry,
	groups []historyGroup,
) grid {
	var g grid
	for _, entries := range [][]state.Entry{open, running, queued, recent} {
		for _, e := range entries {
			g.add(labelOf(e), authorOf(e.Author))
		}
	}
	for _, run := range manual {
		if run.Number == 0 {
			g.add(branchLabel(run.Repo, run.Branch), "")
			continue
		}
		g.add(labelOf(state.Entry{Key: run.Key()}), authorOf(run.Author))
	}
	for _, p := range babysits {
		if p.Number == 0 {
			g.add(branchLabel(p.Repo, p.Branch), "")
			continue
		}
		g.add(labelOf(state.Entry{Key: p.Key()}), authorOf(p.Author))
	}
	for _, group := range groups {
		g.add(runLabelText(group.run), authorOf(group.run.Author))
	}
	return g.fit(w)
}

func (a *app) header(w *ui.Writer) {
	left := "quorum " + a.version
	right := a.agentLine()
	gap := w.Cols() - ui.Cells(left) - ui.Cells(right)
	if gap < 2 {
		w.Printf("%s\n%s\n", w.Bold(left), w.Dim(right))
	} else {
		w.Printf("%s%s%s\n", w.Bold(left), strings.Repeat(" ", gap), w.Dim(right))
	}
	// A config the parser choked on changes what every number below means, so
	// it sits above them rather than in the system section.
	if a.configErr != nil {
		w.Printf("%s\n", w.Yellow("config: "+a.configErr.Error()))
	}
}

// statusbar answers "what is this machine doing right now" in one line, before
// any section has to be read. Everything on it is live state; the settings that
// produced it live in the system section.
func (a *app) statusbar(w *ui.Writer, reviewing, fixing, queued int) {
	slots := fmt.Sprintf("%d/%d", reviewing, a.cfg.MaxConcurrent)
	if reviewing > 0 {
		slots = w.Cyan(slots)
	} else {
		slots = w.Dim(slots)
	}
	parts := []string{
		slots + " " + w.Dim("review"),
		metric(w, fixing, "fix", w.Magenta),
		metric(w, queued, "queued", w.Yellow),
	}
	if load, ok := runner.LoadAvg1(); ok {
		text := fmt.Sprintf("%.1f", load)
		if a.cfg.LoadLimit > 0 && load > a.cfg.LoadLimit {
			text = w.Yellow(text)
		}
		parts = append(parts, text+" "+w.Dim("load"))
	}

	line := "  " + strings.Join(parts, w.Dim("   ·   "))
	// One moving character, and only while there is something to move for. An
	// idle dashboard renders byte for byte the same frame every pass, which is
	// what lets watch skip the repaint entirely. Piped output gets none of it:
	// a spinner frozen at one frame in a log file is just a stray character.
	if reviewing+fixing > 0 && w.Color {
		if gap := w.Cols() - ui.Cells(line) - 1; gap > 1 {
			line += strings.Repeat(" ", gap) + w.Cyan(ui.Spinner(a.tick))
		}
	}
	w.Printf("\n%s\n", line)
	w.Rule()
}

// metric renders one "3 fix" pair, coloured only when the number is not zero.
func metric(w *ui.Writer, n int, label string, style func(string) string) string {
	count := strconv.Itoa(n)
	if n > 0 {
		count = style(count)
	} else {
		count = w.Dim(count)
	}
	return count + " " + w.Dim(label)
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

// openLimit and openWindow bound the open section.
//
// Both exist for the same reason: the state file keeps two hundred records and
// most of them are pull requests that were merged months ago. Whether one is
// still open is only known once GitHub has been asked, and until then age is
// the best guess there is. A run older than the window is history, and the
// history section is where it belongs.
const (
	openLimit  = 10
	openWindow = 14 * 24 * time.Hour
)

// reviewedPRs merges agent state with terminal-run history and keeps the
// newest completed event for each pull request. A newer failed run must hide
// an older successful result, just as a failed agent run does in the state
// file, while a newer successful terminal run replaces stale agent state.
func reviewedPRs(file state.File, runs []history.Run) []state.Entry {
	type event struct {
		at       time.Time
		reviewed bool
		entry    state.Entry
	}
	latest := make(map[string]event)
	for _, e := range file.Entries() {
		if e.Number() == 0 {
			continue
		}
		latest[e.Key] = event{at: e.Time(), reviewed: e.Status == state.OK, entry: e}
	}
	for _, run := range runs {
		if run.Source != history.SourceManual || run.Number() == 0 || run.EndedAt.IsZero() {
			continue
		}
		if current, ok := latest[run.Key]; ok && !run.EndedAt.After(current.at) {
			continue
		}
		succeeded := run.Reviewed && (run.Outcome == history.OK || run.Outcome == history.Converged)
		latest[run.Key] = event{
			at:       run.EndedAt,
			reviewed: succeeded,
			entry: state.Entry{Key: run.Key, Record: state.Record{
				Title:       run.Title,
				Author:      run.Author,
				Status:      state.OK,
				Reason:      run.Reason,
				At:          run.EndedAt.Format(time.RFC3339),
				CommentURL:  run.CommentURL,
				Blockers:    state.Num(run.Blockers),
				Critical:    state.Num(run.Critical),
				Suggestions: state.Num(run.Suggestions),
				Questions:   state.Num(run.Questions),
			}},
		}
	}

	out := make([]state.Entry, 0, len(latest))
	for _, event := range latest {
		if event.reviewed {
			out = append(out, event.entry)
		}
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

// openPRs is what quorum has reviewed and what is still waiting for something
// to happen to it: newest first, minus everything the active section already
// shows, minus everything GitHub says has been merged or closed.
//
// A missing state is not evidence that a pull request is open. It is counted
// separately so the dashboard can say GitHub is still being checked without
// presenting stale reviews as work that is waiting for a person.
func openPRs(reviewed []state.Entry, busy map[string]bool, ends map[string]string) ([]state.Entry, int) {
	cutoff := time.Now().Add(-openWindow)
	out := make([]state.Entry, 0, openLimit)
	checking := 0
	for _, e := range reviewed {
		if busy[e.Key] {
			continue
		}
		if t := e.Time(); t.IsZero() || t.Before(cutoff) {
			continue
		}
		switch ends[e.Key] {
		case gh.StateMerged, gh.StateClosed, gh.StateUnavailable:
			continue
		case gh.StateOpen, gh.StateAutoMerge:
			if len(out) < openLimit {
				out = append(out, e)
			}
		default:
			checking++
		}
	}
	return out, checking
}

// sectionOpen is the top of the dashboard: the pull requests quorum has
// reviewed that are still open, and what the review found.
//
// It answers the question the other two sections cannot. Active is only about
// this minute, and history is a log, so a review from this morning that found
// two blockers scrolls away under later runs even though nobody has done
// anything about it yet. This section keeps it in view until the pull request
// is merged or closed.
func (a *app) sectionOpen(w *ui.Writer, g grid, open []state.Entry, checking int, ends map[string]string) {
	if len(open) == 0 && checking == 0 {
		fmt.Fprintln(w.Out)
		w.Printf("%s %s\n", w.Bold("OPEN"), w.Dim("      nothing reviewed is still open"))
		return
	}
	if len(open) == 0 {
		fmt.Fprintln(w.Out)
		w.Printf("%s %s\n", w.Bold("OPEN"), w.Dim(fmt.Sprintf("      checking GitHub for %d reviewed PR%s", checking, plural(checking))))
		return
	}
	w.Section("open", len(open), 0)
	now := time.Now()
	for _, e := range open {
		a.prLine(w, g, openMark(w, e), e, ends[e.Key])
		w.Printf("      %s\n", openDetail(w, e, now))
	}
	if checking > 0 {
		w.Printf("      %s\n", w.Dim(fmt.Sprintf("checking GitHub for %d more reviewed PR%s", checking, plural(checking))))
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// openMark colours the bullet by what the review found, so the pull request
// that needs a person is the one the eye lands on first.
func openMark(w *ui.Writer, e state.Entry) string {
	switch {
	case e.Reason != "":
		return w.Red("●")
	case e.Blockers > 0:
		return w.Red("●")
	case e.Critical > 0:
		return w.Yellow("●")
	}
	return w.Green("●")
}

// openDetail is the line under an open pull request: when it was reviewed,
// what came out, and the comment that says it.
func openDetail(w *ui.Writer, e state.Entry, now time.Time) string {
	counts := fmt.Sprintf("%dB %dC %dS", e.Blockers, e.Critical, e.Suggestions)
	if e.Blockers == 0 && e.Critical == 0 && e.Suggestions == 0 && e.Questions == 0 {
		counts = "nothing found"
	}
	switch {
	case e.Blockers > 0:
		counts = w.Red(counts)
	case e.Critical > 0:
		counts = w.Yellow(counts)
	default:
		counts = w.Dim(counts)
	}
	text := w.Dim("reviewed "+historyWhen(now, e.Time())+" · ") + counts
	if e.Reason != "" {
		text = w.Red("merge failed") + w.Dim(": "+ui.Truncate(mergeFailureReason(e.Reason), 30)+" · ") + counts
	}
	if e.CommentURL != "" {
		text += w.Dim("  ") + w.Link(w.Blue("comment ↗"), e.CommentURL)
	}
	return text
}

// sectionActive is everything in flight: reviews the agent started, reviews
// started in a terminal, fix loops, and what is waiting for a slot.
//
// An idle machine gets one line rather than a heading. The section still says
// in words that nothing is running, which is what keeps "nothing is happening"
// distinguishable from "this feature is not here", but it does not spend four
// headings and ten lines to say it: the counts behind those headings are on
// the status bar directly above.
func (a *app) sectionActive(
	w *ui.Writer,
	g grid,
	running []state.Entry,
	manual []review.LiveRun,
	babysits []loop.Progress,
	queued []state.Entry,
	live map[string]runner.Marker,
	ends map[string]string,
) {
	if len(running)+len(manual)+len(babysits)+len(queued) == 0 {
		fmt.Fprintln(w.Out)
		w.Printf("%s %s\n", w.Bold("ACTIVE"), w.Dim("    nothing running"))
		return
	}
	w.Section("active", len(live), a.cfg.MaxConcurrent)

	for _, e := range running {
		_, alive := live[e.Key]
		a.prLine(w, g, w.Cyan("●"), e, ends[e.Key])
		if !alive {
			w.Printf("      %s\n", w.Red("process gone, will be retried"))
			continue
		}
		since := ""
		if t := e.Started(); !t.IsZero() {
			since = ui.Duration(time.Since(t)) + " · "
		}
		w.Printf("      %s\n", w.Dim("review · agent · "+since+reviewProgress(e.RunDir, a.cfg.Reviewers)))
	}
	for _, run := range manual {
		if run.Number == 0 {
			branchLine(w, g, w.Magenta("◆"), run.Repo, run.Branch)
			w.Printf("      %s\n", w.Dim("review · manual · "+ui.Duration(time.Since(run.StartedAt))+
				" · "+reviewProgress(run.RunDir, run.Reviewers)))
			continue
		}
		entry := state.Entry{
			Key: run.Key(),
			Record: state.Record{
				Title:     run.Title,
				Author:    run.Author,
				StartedAt: run.StartedAt.Format(time.RFC3339),
				RunDir:    run.RunDir,
			},
		}
		a.prLine(w, g, w.Magenta("◆"), entry, ends[entry.Key])
		w.Printf("      %s\n", w.Dim("review · manual · "+ui.Duration(time.Since(run.StartedAt))+
			" · "+reviewProgress(run.RunDir, run.Reviewers)))
	}
	now := time.Now()
	for _, p := range babysits {
		if p.Number == 0 {
			branchLine(w, g, w.Magenta("●"), p.Repo, p.Branch)
			w.Printf("      %s\n", babysitTrack(w, p, now))
			continue
		}
		e := state.Entry{Key: p.Key(), Record: state.Record{Title: p.Title, Author: p.Author}}
		a.prLine(w, g, w.Magenta("●"), e, ends[p.Key()])
		w.Printf("      %s\n", babysitTrack(w, p, now))
	}
	for _, e := range queued {
		a.prLine(w, g, w.Dim("○"), e, ends[e.Key])
		reason := e.Reason
		if reason == "" {
			reason = "waiting"
		}
		if e.Status == state.Deferred {
			reason = "held back: " + reason
		}
		w.Printf("      %s\n", w.Dim("queued · "+reason))
	}
}

func reviewProgress(runDir string, requested int) string {
	progress := "starting up"
	if p, ok := runner.ReadProgress(runDir, requested); ok {
		progress = fmt.Sprintf("%d/%d reviewers done", p.Done, p.Requested)
		if p.Failed > 0 {
			progress += fmt.Sprintf(", %d failed", p.Failed)
		}
		if p.Done+p.Failed >= p.Requested {
			progress = "aggregating"
		}
	}
	return progress
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
	const indent = 6
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

	room, used := w.Cols()-indent, 0
	var out []string
	for i, text := range plain {
		need := ui.Cells(text)
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
	case loop.PhaseConflictFix:
		what = "resolving merge conflicts"
	case loop.PhaseDivergence:
		what = "analyzing divergence"
	default:
		return "", ""
	}
	if !p.Since.IsZero() {
		what += " ● " + ui.Duration(now.Sub(p.Since))
	}
	return what, w.Cyan(what)
}

// sectionHistory lists finished runs, newest first, and returns the pull
// requests it drew so watch knows whose merge state is worth looking up.
//
// The log is one entry per run, which is the difference that makes this
// section worth reading: the state file keeps one record per pull request, so
// four reviews of the same one used to collapse into a single line showing
// only the last, and a review started in a terminal never reached it at all.
//
// fallback is what the state file still holds. It is used only while the log
// is empty, so upgrading to a build that keeps a log does not start on a blank
// screen; the first logged run takes over from it.
func (a *app) sectionHistory(w *ui.Writer, g grid, fallback []state.Entry, groups []historyGroup, ends map[string]string) []string {
	w.Section("history", 0, 0)
	if len(groups) == 0 && len(fallback) == 0 {
		w.Printf("  %s\n", w.Dim("nothing has finished yet"))
		return nil
	}
	if len(groups) == 0 {
		return a.historyFromState(w, g, fallback, ends)
	}

	now := time.Now()
	keys := make([]string, 0, len(groups))
	lines := make([][]ui.Cell, 0, len(groups))
	for _, group := range groups {
		run := group.run
		if run.Number() > 0 {
			keys = append(keys, run.Key)
		}
		result, style := historyResult(w, run)
		line := historyLine{
			label:      runLabelText(run),
			url:        run.URL(),
			when:       historyWhen(now, run.EndedAt),
			author:     authorOf(run.Author),
			kind:       groupKind(group),
			result:     result,
			resStyle:   style,
			reason:     historyReason(run),
			commentURL: run.CommentURL,
			end:        ends[run.Key],
		}
		line.mark, line.markStyle = historyMark(w, group)
		if group.failed > 0 && group.runs > 1 {
			line.kindStyle = w.Red
		}
		lines = append(lines, line.cells(w, g))
	}
	printTable(w, lines)
	return keys
}

// historyFromState draws the state file's finished records in the same shape.
func (a *app) historyFromState(w *ui.Writer, g grid, recent []state.Entry, ends map[string]string) []string {
	now := time.Now()
	keys := make([]string, 0, len(recent))
	lines := make([][]ui.Cell, 0, len(recent))
	for _, e := range recent {
		keys = append(keys, e.Key)
		line := historyLine{
			mark:      w.SymFail(),
			markStyle: w.Red,
			label:     labelOf(e),
			url:       e.URL(),
			when:      historyWhen(now, e.Time()),
			author:    authorOf(e.Author),
			end:       ends[e.Key],
		}
		switch e.Status {
		case state.OK:
			if e.Reason != "" {
				line.result, line.resStyle = "merge failed", w.Red
				line.reason = mergeFailureReason(e.Reason)
				break
			}
			line.mark, line.markStyle = w.SymOK(), w.Green
			line.result, line.resStyle = findingsText(w, e)
			line.commentURL = e.CommentURL
			if e.CommentURL == "" {
				line.reason = "not posted"
			}
		case state.Failed:
			line.result, line.resStyle = fmt.Sprintf("failed after %d attempt(s)", e.Fails), w.Red
			line.reason = e.Reason
		case state.GaveUp:
			line.result, line.resStyle = "gave up", w.Red
			line.reason = "retry: quorum run " + e.Key
		default:
			line.mark, line.markStyle = "–", w.Dim
			line.result, line.resStyle = "skipped", w.Dim
			line.reason = e.Reason
		}
		lines = append(lines, line.cells(w, g))
	}
	printTable(w, lines)
	return keys
}

// historyGroup is the runs on one pull request that the log holds, collapsed
// into the single line the dashboard draws for it.
//
// The log deliberately keeps one entry per run, because the state file's one
// record per pull request is what used to reduce four reviews of the same
// branch to a single line showing only the last. Collapsing them again on
// screen would give that back, so the line carries the counts: how many runs
// there were, and how many of them failed. A failed attempt followed by a
// successful one is still visible as a failure, which is the part that would
// otherwise be lost.
type historyGroup struct {
	run    history.Run // the newest run, which is what the line reports
	runs   int
	failed int
	fixes  int
}

// groupHistory collapses runs, which arrive newest first, by pull request. The
// order of the groups is the order their newest run appears in, so the section
// still reads newest first.
func groupHistory(runs []history.Run) []historyGroup {
	out := make([]historyGroup, 0, len(runs))
	at := make(map[string]int, len(runs))
	for _, run := range runs {
		i, ok := -1, false
		if run.Key != "" {
			i, ok = at[run.Key]
		}
		if !ok {
			i = len(out)
			out = append(out, historyGroup{run: run})
			if run.Key != "" {
				at[run.Key] = i
			}
		}
		out[i].runs++
		if runFailed(run) {
			out[i].failed++
		}
		if run.Kind == history.KindBabysit {
			out[i].fixes++
		}
	}
	return out
}

// runFailed is what puts a red mark on a run: a failure, a run that gave up,
// and a review that produced a comment but could not be merged.
func runFailed(run history.Run) bool {
	if run.Reviewed && run.Reason != "" {
		return true
	}
	return run.Outcome == history.Failed || run.Outcome == history.GaveUp
}

// historyWhen is the clock time for a run from today and the date for an older
// one. A log is read as a sequence, and "3h ago" next to "4h ago" says less
// about the order of two runs than the times they finished at.
func historyWhen(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if y, m, d := now.Date(); func() bool { ty, tm, td := t.Date(); return ty == y && tm == m && td == d }() {
		return t.Format("15:04")
	}
	return t.Format("2 Jan")
}

// historyMark is the outcome symbol of the newest run in a group, with the
// style it is drawn in. It reports where the pull request stands now rather
// than the worst thing that ever happened to it, because a failure that a later
// run fixed is not what the line is about; the run counts next to it say a
// failure happened at all.
func historyMark(w *ui.Writer, group historyGroup) (string, func(string) string) {
	run := group.run
	if run.Reviewed && run.Reason != "" {
		return w.SymFail(), w.Red
	}
	switch run.Outcome {
	case history.OK, history.Converged:
		return w.SymOK(), w.Green
	case history.Failed, history.GaveUp:
		return w.SymFail(), w.Red
	default:
		return "–", w.Dim
	}
}

// groupKind is the kind column: what kind of work the line stands for, and for
// more than one run how many there were.
//
// A single review says nothing at all, because that is what almost every line
// is and a tag on all of them is a column of noise. Failures are named here
// rather than left to the outcome symbol, which only ever reports the newest
// run.
func groupKind(group historyGroup) string {
	if group.runs == 1 {
		return runKind(group.run)
	}
	text := fmt.Sprintf("%d runs", group.runs)
	if group.fixes == group.runs {
		text = fmt.Sprintf("%d fix runs", group.runs)
	}
	if group.failed > 0 {
		text += fmt.Sprintf(", %d failed", group.failed)
	}
	return text
}

func runKind(run history.Run) string {
	if run.Kind != history.KindBabysit {
		return ""
	}
	switch {
	case run.Rounds == 1:
		return "fix, 1 round"
	case run.Rounds > 1:
		return fmt.Sprintf("fix, %d rounds", run.Rounds)
	}
	return "fix"
}

// historyResult is what the run produced, which is the column the eye lands in.
// It is only the outcome itself; whatever explains it goes to historyReason, so
// that the outcomes of twenty runs line up under each other instead of each
// starting wherever the line before it happened to end.
func historyResult(w *ui.Writer, run history.Run) (string, func(string) string) {
	switch {
	case run.Outcome == history.Skipped:
		return "skipped", w.Dim
	case run.Outcome == history.GaveUp:
		return "gave up", w.Red
	case run.Outcome == history.Failed:
		return "failed", w.Red
	case run.Reviewed && run.Reason != "":
		return "merge failed", w.Red
	case !run.Reviewed:
		return "finished", w.Dim
	}

	// Compact counts, because this is a log: a column of "0 blockers, 1
	// critical, 0 suggestions" is three quarters punctuation, and the same
	// shorthand already labels a fix loop's review round on the line above.
	counts := fmt.Sprintf("%dB %dC %dS", run.Blockers, run.Critical, run.Suggestions)
	if run.Blockers == 0 && run.Critical == 0 && run.Suggestions == 0 && run.Questions == 0 {
		counts = "nothing found"
	}
	switch {
	case run.Blockers > 0:
		return counts, w.Red
	case run.Critical > 0:
		return counts, w.Yellow
	}
	return counts, nil
}

// historyReason explains the result in the words the run recorded.
func historyReason(run history.Run) string {
	switch {
	case run.Outcome == history.Skipped, run.Outcome == history.Failed:
		return run.Reason
	case run.Outcome == history.GaveUp:
		return "retry: quorum run " + run.Key
	case run.Reviewed && run.Reason != "":
		return mergeFailureReason(run.Reason)
	}
	return ""
}

func mergeFailureReason(reason string) string {
	if strings.HasPrefix(reason, "auto-merge failed") {
		if _, detail, ok := strings.Cut(reason, ": "); ok {
			return detail
		}
	}
	return reason
}

// reasonWidth caps how much of a recorded reason the table asks for. Reasons
// are a sentence and the columns behind them are a word, so an unbounded one
// would take the room those need and give the eye nothing back for it.
const (
	reasonWidth = 44
	reasonFloor = 24
)

// historyLine is one row of the history table, in the order the columns are
// drawn: outcome, time, pull request, author, kind, result, comment, how the
// pull request ended, and last the reason.
//
// The reason comes after the two columns it looks out of place before, because
// a run either succeeded and has a comment and an end state, or failed and has
// a reason instead. Putting the long column between them left every successful
// line padding out forty empty cells to reach its own comment link.
type historyLine struct {
	mark       string
	markStyle  func(string) string
	when       string
	label      string
	url        string
	author     string
	kind       string
	kindStyle  func(string) string
	result     string
	resStyle   func(string) string
	reason     string
	commentURL string
	end        string
}

func (l historyLine) cells(w *ui.Writer, g grid) []ui.Cell {
	kindStyle := l.kindStyle
	if kindStyle == nil {
		kindStyle = w.Dim
	}
	label := ui.Cell{Text: l.label, Cut: truncateLabel, Want: g.label,
		Flex: true, Give: giveLabel, Min: labelFloor, Style: func(s string) string {
			styled := w.Bold(s)
			if l.end == gh.StateMerged {
				styled = w.Strike(styled)
			}
			return w.Link(styled, l.url)
		}}
	comment := ui.Cell{}
	if l.commentURL != "" {
		comment = ui.Cell{Text: "comment ↗", Style: func(s string) string {
			return w.Link(w.Blue(s), l.commentURL)
		}}
	}
	return []ui.Cell{
		{Text: l.mark, Style: l.markStyle},
		{Text: l.when, Style: w.Dim},
		label,
		{Text: l.author, Want: g.author, Flex: true, Give: giveAuthor, Style: w.Dim},
		{Text: l.kind, Style: kindStyle},
		{Text: l.result, Style: l.resStyle},
		comment,
		{Text: endWord(l.end), Style: w.Dim},
		{Text: ui.Truncate(l.reason, reasonWidth), Style: w.Dim, Flex: true, Give: giveDetail, Min: reasonFloor},
	}
}

// printTable lays the rows out and prints them under the two space indent every
// section body uses.
func printTable(w *ui.Writer, rows [][]ui.Cell) {
	const indent = "  "
	for _, line := range ui.Columns(rows, 2, max(w.Cols()-len(indent), 1)) {
		w.Printf("%s%s\n", indent, line)
	}
}

// footer is the configuration the numbers above were produced under, on one
// dim line. It used to be a section of its own, which gave four settings that
// change once a month the same weight as the work in flight.
func (a *app) footer(w *ui.Writer) {
	scope := "every repo that asks you"
	if len(a.cfg.Orgs) > 0 || len(a.cfg.Repos) > 0 {
		scope = strings.Join(append(append([]string{}, a.cfg.Orgs...), a.cfg.Repos...), " ")
	}
	parts := []string{
		scope,
		fmt.Sprintf("%d at a time, %d reviewers each", a.cfg.MaxConcurrent, a.cfg.Reviewers),
		autoMergeSummary(a.cfg),
	}
	if a.cfg.LoadLimit > 0 {
		parts = append(parts, fmt.Sprintf("held back above load %.0f", a.cfg.LoadLimit))
	}
	if limit := a.budgetBytes(); limit > 0 {
		parts = append(parts, ui.Bytes(limit)+" cache")
	}
	fmt.Fprintln(w.Out)
	w.Printf("%s\n", w.Dim(ui.Truncate(strings.Join(parts, " · "), w.Cols())))
}

func autoMergeSummary(cfg config.Config) string {
	var sources []string
	if cfg.AutoMergeAgent {
		sources = append(sources, "agent")
	}
	if cfg.AutoMergeReview {
		sources = append(sources, "review")
	}
	if cfg.AutoMergeBabysit {
		sources = append(sources, "babysit")
	}
	if len(sources) == 0 {
		return "auto-merge off"
	}
	return "auto-merge " + strings.Join(sources, "+") + " (repo method)"
}

func runLabelText(run history.Run) string {
	if run.Branch != "" {
		return fmt.Sprintf("%s %s", run.Name(), run.Branch)
	}
	return fmt.Sprintf("%s #%d", run.Name(), run.Number())
}

// findingsText is the one line summary of a finished review from the state
// file.
func findingsText(w *ui.Writer, e state.Entry) (string, func(string) string) {
	counts := fmt.Sprintf("%d blockers, %d critical, %d suggestions", e.Blockers, e.Critical, e.Suggestions)
	if e.Blockers == 0 && e.Critical == 0 && e.Suggestions == 0 && e.Questions == 0 {
		counts = "nothing found"
	}
	if e.Blockers > 0 {
		return counts, w.Red
	}
	if e.Critical > 0 {
		return counts, w.Yellow
	}
	return counts, nil
}

// grid is the width of the two columns every section shares: the repository and
// number a line is found by, and the author beside it.
//
// It is measured once from every row the frame is about to draw, and it is the
// whole reason the screen reads as a table. The widths used to be two constants
// picked per line, one for a row with an author and a wider one for a row
// without, so every result and every title on the screen sat at one of two
// offsets depending on whether GitHub had told us who opened the pull request.
type grid struct {
	label  int
	author int
}

// labelFloor is the least a label column may be squeezed to before what is left
// stops naming a pull request at all. The caps keep one very long repository
// name or login from taking the room the result columns need, and both are
// bounded again by the terminal so that a narrow one is not spent entirely on
// the two leftmost columns.
const (
	labelFloor = 12
	labelCap   = 34
	authorCap  = 18
)

// What a narrow terminal costs, in the order it is paid. The author goes first
// because every other column says something about the run itself, and the one
// column that must survive is the one naming the pull request the line is
// about.
const (
	giveAuthor = iota
	giveDetail
	giveLabel
)

func (g *grid) add(label, author string) {
	g.label = max(g.label, ui.Cells(label))
	g.author = max(g.author, ui.Cells(author))
}

// fit bounds the measured widths, once every row has been added.
func (g *grid) fit(w *ui.Writer) grid {
	cols := max(w.Cols(), 1)
	return grid{
		label:  min(g.label, labelCap, cols/3),
		author: min(g.author, authorCap, cols/6),
	}
}

func labelOf(e state.Entry) string {
	return fmt.Sprintf("%s #%d", e.Name(), e.Number())
}

func authorOf(author string) string {
	if author == "" {
		return ""
	}
	return "@" + author
}

// truncateLabel shortens a label to width while keeping the number, which is
// what the label is looked up by. Losing the tail of a repository name still
// leaves a line that can be identified; losing "#2074" does not.
func truncateLabel(label string, width int) string {
	if ui.Cells(label) <= width {
		return label
	}
	if name, number, ok := strings.Cut(label, " #"); ok {
		suffix := " #" + number
		if suffixWidth := ui.Cells(suffix); suffixWidth < width {
			return ui.Truncate(name, width-suffixWidth) + suffix
		}
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
	case gh.StateAutoMerge:
		return "auto-merge queued"
	}
	return ""
}

// prLine prints "  ● toaster-api #2017  @example-user  the title", with the
// repository and number linking to the pull request.
//
// It draws the same first columns as the history table and at the same widths,
// so what is running and what has finished can be read down the screen as one
// list rather than as three blocks that each chose their own offsets.
func (a *app) prLine(w *ui.Writer, g grid, mark string, e state.Entry, end string) {
	printTable(w, [][]ui.Cell{prCells(w, g, mark, labelOf(e), e.URL(), e.Author, e.Title, end)})
}

// branchLine is the same row for a run on a branch, which has no number and no
// author to put in the columns.
func branchLine(w *ui.Writer, g grid, mark, repo, branch string) {
	printTable(w, [][]ui.Cell{prCells(w, g, mark, branchLabel(repo, branch), "", "", "", "")})
}

func branchLabel(repo, branch string) string {
	_, name, ok := strings.Cut(repo, "/")
	if !ok {
		name = repo
	}
	return name + " " + branch
}

func prCells(w *ui.Writer, g grid, mark, label, url, author, title, end string) []ui.Cell {
	return []ui.Cell{
		{Text: mark},
		{Text: label, Cut: truncateLabel, Want: g.label,
			Flex: true, Give: giveLabel, Min: labelFloor, Style: func(s string) string {
				styled := w.Bold(s)
				if end == gh.StateMerged {
					styled = w.Strike(styled)
				}
				return w.Link(styled, url)
			}},
		{Text: authorOf(author), Want: g.author, Flex: true, Give: giveAuthor, Style: w.Dim},
		{Text: title, Flex: true, Give: giveDetail, Min: titleFloor},
		{Text: endWord(end), Style: w.Dim},
	}
}

// titleFloor is the least of a title worth printing. Below it the column says
// nothing about the pull request and the room is better spent on the columns
// that identify it.
const titleFloor = 12

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
	path   string
}

func newEndStates(path string) *endStates {
	return &endStates{state: readEndStateCache(path), change: make(chan struct{}, 1), path: path}
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
	e.state = maps.Clone(states)
	snapshot := maps.Clone(e.state)
	path := e.path
	e.mu.Unlock()
	_ = writeEndStateCache(path, snapshot)
}

// trackEnds keeps the merge state of relevant pull requests roughly current.
//
// It runs beside the redraw and never inside it. The dashboard repaints every
// few seconds, and asking GitHub at that rate would make the screen wait on the
// network and earn a rate limit for a fact that only changes when somebody
// presses a button. One batched query covers every tracked pull request.
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
		if err := refreshEndStates(ctx, client, e, keys); err != nil {
			// Nothing on the screen depends on this, so a failed lookup keeps
			// the previous answer rather than blanking what it already knows.
			continue
		}
	}
}

func refreshEndStates(ctx context.Context, client *gh.Client, e *endStates, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	states, err := client.PRStates(ctx, keys)
	if err != nil {
		return err
	}
	e.store(states)
	return nil
}

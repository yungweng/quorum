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

	// What is running gets as much room as it needs and no more, and the log of
	// finished runs gets the rest.
	//
	// The four fixed sections this replaced spent ten lines saying "nothing"
	// four times over, which is what an idle machine looks like almost all of
	// the time, while the runs that had actually happened were squeezed in
	// underneath. The counts that used to justify those headings are on the
	// status bar, so nothing is lost by collapsing them.
	a.sectionActive(w, running, manualReviews, babysits, queued, live, ends)
	past := a.sectionHistory(w, recent, ends)
	a.footer(w)

	shown := make([]string, 0, len(running)+len(manualReviews)+len(queued)+len(past)+len(babysits))
	shownSet := make(map[string]bool, cap(shown))
	for _, group := range [][]state.Entry{running, queued} {
		for _, e := range group {
			if shownSet[e.Key] {
				continue
			}
			shown = append(shown, e.Key)
			shownSet[e.Key] = true
		}
	}
	for _, key := range past {
		if !shownSet[key] {
			shown = append(shown, key)
			shownSet[key] = true
		}
	}
	for _, run := range manualReviews {
		key := run.Key()
		if !shownSet[key] {
			shown = append(shown, key)
			shownSet[key] = true
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
		a.prLine(w, w.Cyan("●"), e, ends[e.Key])
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
		entry := state.Entry{
			Key: run.Key(),
			Record: state.Record{
				Title:     run.Title,
				StartedAt: run.StartedAt.Format(time.RFC3339),
				RunDir:    run.RunDir,
			},
		}
		a.prLine(w, w.Magenta("◆"), entry, ends[entry.Key])
		w.Printf("      %s\n", w.Dim("review · manual · "+ui.Duration(time.Since(run.StartedAt))+
			" · "+reviewProgress(run.RunDir, run.Reviewers)))
	}
	now := time.Now()
	for _, p := range babysits {
		e := state.Entry{Key: p.Key(), Record: state.Record{Title: p.Title}}
		a.prLine(w, w.Magenta("●"), e, ends[p.Key()])
		w.Printf("      %s\n", babysitTrack(w, p, now))
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
func (a *app) sectionHistory(w *ui.Writer, fallback []state.Entry, ends map[string]string) []string {
	limit := a.cfg.History
	if limit <= 0 {
		limit = config.Default().History
	}
	runs := history.Read(a.p.HistoryFile, limit)

	w.Section("history", 0, 0)
	if len(runs) == 0 && len(fallback) == 0 {
		w.Printf("  %s\n", w.Dim("nothing has finished yet"))
		return nil
	}
	if len(runs) == 0 {
		return a.historyFromState(w, fallback, limit, ends)
	}

	keys := make([]string, 0, len(runs))
	now := time.Now()
	for _, run := range runs {
		keys = append(keys, run.Key)
		w.Printf("  %s %s %s%s %s%s\n",
			historyMark(w, run),
			w.Dim(ui.Pad(historyWhen(now, run.EndedAt), 6)),
			runLabel(w, run, ends[run.Key]),
			runLabelPad(run),
			historyDetail(w, run),
			endSuffix(w, ends[run.Key]))
	}
	return keys
}

// historyFromState draws the state file's finished records in the same shape.
func (a *app) historyFromState(w *ui.Writer, recent []state.Entry, limit int, ends map[string]string) []string {
	if len(recent) > limit {
		recent = recent[:limit]
	}
	now := time.Now()
	keys := make([]string, 0, len(recent))
	for _, e := range recent {
		keys = append(keys, e.Key)
		var symbol, detail string
		switch e.Status {
		case state.OK:
			symbol, detail = w.Green(w.SymOK()), findingsText(w, e)
		case state.Failed:
			symbol = w.Red(w.SymFail())
			detail = w.Red(fmt.Sprintf("failed after %d attempt(s)", e.Fails))
			if e.Reason != "" {
				detail += w.Dim(", " + ui.Truncate(e.Reason, 40))
			}
		case state.GaveUp:
			symbol = w.Red(w.SymFail())
			detail = w.Red("gave up") + w.Dim(", retry: quorum run "+e.Key)
		default:
			symbol, detail = w.Dim("–"), w.Dim("skipped: "+e.Reason)
		}
		w.Printf("  %s %s %s%s %s%s\n",
			mark(w, symbol), w.Dim(ui.Pad(historyWhen(now, e.Time()), 6)),
			prLabel(w, e, ends[e.Key]), labelPad(e), detail, endSuffix(w, ends[e.Key]))
	}
	return keys
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

func historyMark(w *ui.Writer, run history.Run) string {
	switch run.Outcome {
	case history.OK, history.Converged:
		return mark(w, w.Green(w.SymOK()))
	case history.Failed, history.GaveUp:
		return mark(w, w.Red(w.SymFail()))
	default:
		return mark(w, w.Dim("–"))
	}
}

// mark pads an outcome symbol to one column width.
//
// With a terminal every symbol is a single cell and this does nothing. Without
// one they degrade to "ok:" and "FAIL:", which are three and five cells, and an
// unpadded mark shifts every column after it on the failing lines only, which
// is the state a piped log is read in.
func mark(w *ui.Writer, symbol string) string {
	return ui.Pad(symbol, max(ui.Cells(w.SymOK()), ui.Cells(w.SymFail())))
}

// historyDetail is what the run produced, which is the column the eye lands in.
func historyDetail(w *ui.Writer, run history.Run) string {
	var text string
	switch {
	case run.Outcome == history.Skipped:
		return w.Dim("skipped: " + run.Reason)
	case run.Outcome == history.GaveUp:
		return w.Red("gave up") + w.Dim(", retry: quorum run "+run.Key)
	case run.Outcome == history.Failed:
		text = w.Red("failed")
		if run.Reason != "" {
			text += w.Dim(", " + ui.Truncate(run.Reason, 44))
		}
		return prefixKind(w, run) + text
	case !run.Reviewed:
		return prefixKind(w, run) + w.Dim("finished")
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
		counts = w.Red(counts)
	case run.Critical > 0:
		counts = w.Yellow(counts)
	}
	text = prefixKind(w, run) + counts
	if run.CommentURL != "" {
		return text + "  " + w.Link(w.Blue("comment ↗"), run.CommentURL)
	}
	return text
}

// prefixKind marks a fix loop. A review says nothing, because that is what
// almost every line is and a tag on all of them is a column of noise.
func prefixKind(w *ui.Writer, run history.Run) string {
	if run.Kind != history.KindBabysit {
		return ""
	}
	if run.Rounds == 1 {
		return w.Dim("fix, 1 round · ")
	}
	if run.Rounds > 1 {
		return w.Dim(fmt.Sprintf("fix, %d rounds · ", run.Rounds))
	}
	return w.Dim("fix · ")
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

// runLabel renders the "insura #103" column for a logged run, linked to the
// pull request and struck through once it has been merged.
func runLabel(w *ui.Writer, run history.Run, end string) string {
	label := fmt.Sprintf("%s #%d", run.Name(), run.Number())
	styled := w.Bold(label)
	if end == gh.StateMerged {
		styled = w.Strike(styled)
	}
	return w.Link(styled, run.URL())
}

func runLabelPad(run history.Run) string {
	label := fmt.Sprintf("%s #%d", run.Name(), run.Number())
	if n := labelWidth - ui.Cells(label); n > 0 {
		return strings.Repeat(" ", n)
	}
	return ""
}

// findingsText is the one line summary of a finished review from the state
// file, with the posted comment behind a link.
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
	if n := labelWidth - ui.Cells(labelOf(e)); n > 0 {
		return strings.Repeat(" ", n)
	}
	return ""
}

func truncateLabel(e state.Entry, width int) string {
	label := labelOf(e)
	if ui.Cells(label) <= width {
		return label
	}
	suffix := fmt.Sprintf(" #%d", e.Number())
	if suffixWidth := ui.Cells(suffix); suffixWidth < width {
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
		endWidth = 2 + ui.Cells(word)
	}
	if e.Title == "" {
		// Records written before titles were stored have nothing to show, and
		// padding to an empty column just leaves trailing whitespace.
		label := truncateLabel(e, max(w.Cols()-4-endWidth, 1))
		w.Printf("  %s %s%s\n", mark, styledPRLabel(w, e, label, end), endSuffix(w, end))
		return
	}
	available := max(w.Cols()-6-endWidth, 1)
	labelRoom := max(labelWidth, ui.Cells(labelOf(e)))
	titleRoom := available - labelRoom
	if titleRoom < 12 {
		labelRoom = max(available-12, 1)
		titleRoom = max(available-labelRoom, 0)
	}
	label := truncateLabel(e, labelRoom)
	w.Printf("  %s %s%s  %s%s\n",
		mark,
		styledPRLabel(w, e, label, end),
		strings.Repeat(" ", max(labelRoom-ui.Cells(label), 0)),
		ui.Truncate(e.Title, titleRoom),
		endSuffix(w, end))
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

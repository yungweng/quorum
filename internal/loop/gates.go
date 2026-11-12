package loop

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yungweng/quorum/internal/rundir"
)

// questionGate handles a step that ended with OPEN QUESTIONS.
//
// In autonomous mode the session was told not to ask, so the questions go
// straight back for it to decide itself, a bounded number of times. Giving up
// after that is deliberate: a session that keeps asking despite being told to
// decide is stuck, and looping on it burns money without progress.
func (r *run) questionGate(tagBase string) error {
	for n := 1; hasMarker(r.lastMsg, markerQuestions); n++ {
		questions := section(r.lastMsg, markerQuestions)
		r.rep.Questions(questions)

		if !r.o.Interactive {
			if n > maxQuestionBounces {
				r.rep.Notify("Festgefahren", "Codex stellt weiter Fragen im autonomen Modus")
				return fmt.Errorf("%w: Codex keeps asking questions although autonomous mode told it to decide itself; a human is needed: %s",
					ErrGateAborted, r.targetReference())
			}
			r.rep.Info("autonomous mode: sending the questions back for Codex to decide itself")
			if err := r.resume(fmt.Sprintf("%s-answers-%d", tagBase, n), bounceQuestionsPrompt); err != nil {
				return err
			}
			continue
		}

		r.rep.Notify("Offene Fragen", "Codex braucht eine Entscheidung von dir")
		answers, err := r.readBlock("Answer the questions. Finish with an empty line; 'q' aborts the pipeline.")
		if err != nil {
			return err
		}
		if answers == "" {
			r.rep.Info("No answer given; the questions are shown again.")
			continue
		}
		if err := r.resume(fmt.Sprintf("%s-answers-%d", tagBase, n), answersPrompt(answers)); err != nil {
			return err
		}
	}
	return nil
}

// disputeGate handles a fix round that produced no commits but claims the
// remaining Blocker/Critical findings are wrong.
//
// A dispute is never accepted on first sight. In autonomous mode the session
// must survive one forced adversarial re-check; in interactive mode a human
// accepts it. Without that, any round could be ended by declaring the findings
// wrong, and the run would finish on the fix agent's own say-so.
//
// It returns the accepted rebuttal, or the head of the new commits a re-check
// produced, or an error. The outcome belongs to the round that asked: an
// acceptance never carries into a later round.
func (r *run) disputeGate(step fixOutcome, preSHA string) (disputeOutcome, error) {
	msg := step.last
	for n := 1; ; n++ {
		dispute := section(msg, markerDisputed)
		if markerContent(dispute, markerDisputed) == "" {
			return disputeOutcome{}, fmt.Errorf("%w: dispute marker has no rebuttal", ErrNoProgress)
		}
		r.rep.Dispute(dispute)
		tag := fmt.Sprintf("%s-dispute-%d", step.tag, n)

		var recheck fixOutcome
		var err error
		if !r.o.Interactive {
			if n >= 2 {
				r.rep.Info("dispute upheld after the re-check, rebuttal accepted")
				return disputeOutcome{accepted: true, text: dispute}, nil
			}
			r.rep.Info("autonomous mode: forcing one adversarial re-check")
			recheck, err = r.resumeStep(tag, forceRecheckPrompt)
		} else {
			r.rep.Notify("Disputed findings", "Codex haelt verbleibende Findings fuer falsch")
			reply, action, readErr := r.readDisputeReply()
			if readErr != nil {
				return disputeOutcome{}, readErr
			}
			switch action {
			case disputeAccept:
				return disputeOutcome{accepted: true, text: dispute}, nil
			case disputeAbort:
				return disputeOutcome{}, fmt.Errorf("%w: aborted at the dispute gate", ErrGateAborted)
			}
			if reply == "" {
				r.rep.Info("No input given; the disputed findings are shown again.")
				continue
			}
			recheck, err = r.resumeStep(tag, disputeReplyPrompt(reply))
		}
		if err != nil {
			return disputeOutcome{}, err
		}
		// The re-check turned up a real issue after all, so the loop continues
		// normally.
		if recheck.head != preSHA {
			return disputeOutcome{head: recheck.head, last: recheck.last}, nil
		}
		// No commits and no dispute either: the same stuck state the no-progress
		// stop exists for.
		msg = recheck.last
		if !hasMarker(msg, markerDisputed) {
			r.rep.Notify("Festgefahren", "Dispute-Runde ohne Commits und ohne Dispute")
			return disputeOutcome{}, fmt.Errorf("%w: Codex neither fixed nor still disputes the remaining findings; a human is needed: %s",
				ErrNoProgress, r.targetReference())
		}
	}
}

// disputeOutcome is how one round's dispute gate ended: accepted with the
// rebuttal text, or with head at the commits a re-check produced and last the
// re-check's newest message.
type disputeOutcome struct {
	accepted bool
	text     string
	head     string
	last     string
}

type disputeAction int

const (
	disputeReply disputeAction = iota
	disputeAccept
	disputeAbort
)

func (r *run) readDisputeReply() (string, disputeAction, error) {
	sc, err := r.scanner()
	if err != nil {
		return "", disputeAbort, err
	}
	r.rep.Prompt("Codex disputes the remaining Blocker/Critical findings instead of changing code.\n" +
		"Type 'a' to accept the rebuttals and finish the run, 'q' to abort,\n" +
		"or type a reply to Codex (finish with an empty line).")

	var lines []string
	for sc.Scan() {
		line := sc.Text()
		if len(lines) == 0 {
			switch line {
			case "a":
				return "", disputeAccept, nil
			case "q":
				return "", disputeAbort, nil
			}
		}
		if line == "" {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), disputeReply, nil
}

// readBlock reads lines until an empty one. A "q" line aborts.
func (r *run) readBlock(instructions string) (string, error) {
	sc, err := r.scanner()
	if err != nil {
		return "", err
	}
	r.rep.Prompt(instructions)

	var lines []string
	for sc.Scan() {
		line := sc.Text()
		if line == "q" {
			return "", fmt.Errorf("%w: aborted at the question gate", ErrGateAborted)
		}
		if line == "" {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

// scanner returns the input reader for interactive gates. Without one, the gate
// aborts with a clear message rather than spinning on an immediate EOF.
func (r *run) scanner() (*bufio.Scanner, error) {
	if r.p.In == nil {
		r.rep.Notify("Gate", "Interaktive Eingabe noetig, aber kein Terminal")
		return nil, fmt.Errorf("%w: interactive input required but stdin is not a terminal; rerun in an interactive terminal",
			ErrGateAborted)
	}
	if r.stdin == nil {
		r.stdin = bufio.NewScanner(r.p.In)
	}
	return r.stdin, nil
}

// setupDirenv allows the PR head's .envrc files and records what they looked
// like, so a later change by the fix session is noticed.
func (r *run) setupDirenv() error {
	files, err := rundir.AllowEnvrc(r.ctx, r.p.Git, r.env, "origin/"+r.pr.BaseRefName, r.o.AllowEnvrcChange,
		func() error { return r.p.Git.FetchBranch(r.ctx, r.o.RepoRoot, r.pr.BaseRefName) }, r.rep.Info)
	if err != nil || len(files) == 0 {
		return err
	}
	r.envrcBaseline = map[string]bool{}
	for _, f := range files {
		r.envrcBaseline[f] = true
	}
	r.direnvActive = true
	r.envrcStamp = r.stampEnvrc()
	return nil
}

// checkEnvrc runs before every fix-session step.
//
// The PR head's .envrc files were allowed at startup. This watches those and
// any new non-ignored .envrc, but skips files generated later inside ignored
// caches such as .devbox: direnv never approved or loads those.
//
// In autonomous mode the change is allowed automatically. That is not laziness:
// with the sandbox bypassed the session can already execute anything, so gating
// its own .envrc edit adds no protection at all.
func (r *run) checkEnvrc() error {
	if !r.direnvActive {
		return nil
	}
	now := r.stampEnvrc()
	if now == r.envrcStamp {
		return nil
	}
	diff := r.p.Git.Diff(r.ctx, r.worktree, r.pr.HeadRefOid, ".envrc", ":(glob)**/.envrc")
	r.rep.EnvrcChanged(diff)

	if !r.o.Interactive {
		r.rep.Info("autonomous mode: running direnv allow on the changed .envrc")
		r.rep.Notify("direnv", ".envrc geaendert, automatisch freigegeben")
		if err := r.env.Allow(r.ctx); err != nil {
			return fmt.Errorf("direnv allow: %w", err)
		}
		r.envrcStamp = r.stampEnvrc()
		return nil
	}

	r.rep.Notify("direnv", ".envrc wurde veraendert, Freigabe noetig")
	sc, err := r.scanner()
	if err != nil {
		return err
	}
	r.rep.Prompt("Run direnv allow for the changed .envrc? [y/N] ")
	if !sc.Scan() {
		return fmt.Errorf("%w: no answer at the .envrc gate", ErrGateAborted)
	}
	if reply := strings.TrimSpace(sc.Text()); reply != "y" && reply != "Y" {
		return fmt.Errorf("%w: refusing to continue with an unapproved .envrc change", ErrGateAborted)
	}
	if err := r.env.Allow(r.ctx); err != nil {
		return fmt.Errorf("direnv allow: %w", err)
	}
	r.envrcStamp = r.stampEnvrc()
	return nil
}

// stampEnvrc hashes the .envrc files that matter: those present at startup, and
// any new one that git does not ignore.
func (r *run) stampEnvrc() string {
	var lines []string
	for _, rel := range rundir.FindEnvrc(r.worktree) {
		if !r.envrcBaseline[rel] && r.p.Git.IsIgnored(r.ctx, r.worktree, rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.worktree, rel))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+rel)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

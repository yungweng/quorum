package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/target"
)

const runTargetSchema = 1

// runTarget preserves the target contract that produced a run's reviewer
// outputs. A resume must aggregate and publish under that same contract.
type runTarget struct {
	Schema     int    `json:"schema"`
	Repo       string `json:"repo"`
	Number     int    `json:"number,omitempty"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	BranchOnly bool   `json:"branch_only"`
}

func (r *Runner) resolveRunTarget(ctx context.Context, o *Options) (target.Target, runPaths, error) {
	var run runPaths
	var saved *runTarget
	legacy := false

	if o.ResumeRun != "" {
		var err error
		run, err = o.runDir(0, "")
		if err != nil {
			return target.Target{}, runPaths{}, err
		}
		meta, err := readRunTarget(run.target)
		if errors.Is(err, os.ErrNotExist) {
			legacy = true
		} else if err != nil {
			return target.Target{}, runPaths{}, err
		} else {
			if err := applyRunTarget(o, meta); err != nil {
				return target.Target{}, runPaths{}, err
			}
			saved = &meta
		}
	}

	tgt, err := target.Resolve(ctx, r.GH, r.Git, o.RepoRoot, o.Number, o.Branch, o.BaseBranch)
	if err != nil {
		return target.Target{}, runPaths{}, err
	}
	if o.HeadSHA != "" {
		if !tgt.BranchOnly {
			return target.Target{}, runPaths{}, fmt.Errorf("a pinned head SHA requires a branch-only target")
		}
		if tgt.PR.HeadRefOid != o.HeadSHA {
			return target.Target{}, runPaths{}, fmt.Errorf(
				"%w: pipeline worktree is %s, origin/%s is %s",
				ErrHeadDrifted, o.HeadSHA, tgt.PR.HeadRefName, tgt.PR.HeadRefOid,
			)
		}
	}
	if o.ResumeRun == "" {
		run, err = o.runDir(tgt.PR.Number, tgt.PR.HeadRefName)
		if err != nil {
			return target.Target{}, runPaths{}, err
		}
	}
	if saved != nil {
		if err := saved.matches(tgt); err != nil {
			return target.Target{}, runPaths{}, err
		}
	}
	if legacy && !legacyPRRunMatches(run.root, tgt) {
		return target.Target{}, runPaths{}, fmt.Errorf(
			"resume run has no target metadata, so its original branch target and base cannot be verified; start a new review",
		)
	}
	return tgt, run, nil
}

func newRunTarget(o Options, tgt target.Target, baseBranch string) runTarget {
	return runTarget{
		Schema:     runTargetSchema,
		Repo:       o.Repo,
		Number:     tgt.PR.Number,
		Branch:     tgt.PR.HeadRefName,
		BaseBranch: baseBranch,
		BranchOnly: tgt.BranchOnly,
	}
}

func applyRunTarget(o *Options, meta runTarget) error {
	if err := meta.validate(); err != nil {
		return err
	}
	if o.Repo != meta.Repo {
		return fmt.Errorf("resume run belongs to %s, not %s", meta.Repo, o.Repo)
	}
	if o.Number != 0 && o.Number != meta.Number {
		return fmt.Errorf("resume run targets PR #%d, not PR #%d", meta.Number, o.Number)
	}
	if o.Branch != "" && o.Branch != meta.Branch {
		return fmt.Errorf("resume run targets branch %s, not %s", meta.Branch, o.Branch)
	}
	if o.BaseBranch != "" && o.BaseBranch != meta.BaseBranch {
		return fmt.Errorf("resume run uses base %s, not %s", meta.BaseBranch, o.BaseBranch)
	}
	o.Number = meta.Number
	o.Branch = meta.Branch
	o.BaseBranch = meta.BaseBranch
	return nil
}

func (m runTarget) validate() error {
	if m.Schema != runTargetSchema {
		return fmt.Errorf("resume target metadata has schema %d, want %d", m.Schema, runTargetSchema)
	}
	if m.Repo == "" || m.Branch == "" || m.BaseBranch == "" {
		return fmt.Errorf("resume target metadata is incomplete")
	}
	if m.BranchOnly == (m.Number > 0) {
		return fmt.Errorf("resume target metadata has inconsistent target kind")
	}
	return nil
}

func (m runTarget) matches(tgt target.Target) error {
	if m.BranchOnly != tgt.BranchOnly ||
		m.Number != tgt.PR.Number ||
		m.Branch != tgt.PR.HeadRefName {
		return fmt.Errorf("resolved target differs from the target saved for this run")
	}
	return nil
}

func readRunTarget(path string) (runTarget, error) {
	var meta runTarget
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("read resume target metadata: %w", err)
	}
	if err := meta.validate(); err != nil {
		return meta, err
	}
	return meta, nil
}

func writeRunTarget(path string, meta runTarget) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// Runs created before target metadata existed can only be resumed when their
// generated name proves they were for the same explicit PR. A branch run has
// no durable record of its base, so guessing would risk changing review mode.
func legacyPRRunMatches(root string, tgt target.Target) bool {
	if tgt.BranchOnly || tgt.PR.Number <= 0 {
		return false
	}
	name := filepath.Base(root)
	const stampLen = len("20060102-150405")
	if len(name) <= stampLen || name[len(name)-stampLen-1] != '-' {
		return false
	}
	stamp := name[len(name)-stampLen:]
	if _, err := time.Parse("20060102-150405", stamp); err != nil {
		return false
	}
	stem := strings.TrimSuffix(name, "-"+stamp)
	if strings.HasSuffix(stem, "-branch-"+legacySafeRunPart(tgt.PR.HeadRefName)) {
		return false
	}
	return strings.HasSuffix(stem, fmt.Sprintf("-pr-%d", tgt.PR.Number))
}

func legacySafeRunPart(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	s = strings.ReplaceAll(s, "..", "-")
	if s == "" {
		return "unknown"
	}
	return s
}

// Package envexec runs commands inside a worktree, optionally through direnv.
//
// Both the review core and the fix pipeline have to run every command in the
// project's own environment: a repository that installs its toolchain from a
// direnv or devbox hook has no usable node, python or test runner without it.
// The shell tools each carried their own `cd "$WORKTREE" && direnv exec ...`
// wrapper; this is that wrapper, once.
package envexec

import (
	"context"
	"io"
	"time"

	"github.com/yungweng/quorum/internal/proc"
)

// Env is a worktree plus the decision whether direnv wraps commands in it.
type Env struct {
	Worktree string
	Direnv   bool
	// DirenvBin is the resolved direnv path. Empty falls back to a PATH lookup,
	// which is wrong under launchd and right everywhere else.
	DirenvBin string
}

// Cmd is what callers describe; Env decides how it actually gets executed.
type Cmd struct {
	Name   string
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes cmd in the worktree with the given timeout (0 disables it).
func (e Env) Run(ctx context.Context, timeout time.Duration, c Cmd) error {
	name, args := e.wrap(c.Name, c.Args)
	return proc.Run(ctx, timeout, proc.Spec{
		Name:   name,
		Args:   args,
		Dir:    e.Worktree,
		Stdin:  c.Stdin,
		Stdout: c.Stdout,
		Stderr: c.Stderr,
	})
}

// wrap turns a command into its direnv equivalent when direnv is active.
// `direnv exec DIR cmd args` loads DIR's environment and then execs, which is
// why the worktree path appears both as the working directory and as the
// argument: direnv resolves the .envrc from the argument, not from the cwd.
func (e Env) wrap(name string, args []string) (string, []string) {
	if !e.Direnv {
		return name, args
	}
	bin := e.DirenvBin
	if bin == "" {
		bin = "direnv"
	}
	return bin, append([]string{"exec", e.Worktree, name}, args...)
}

// Allow runs `direnv allow` in the worktree. Callers gate this on their own
// .envrc checks; it deliberately does not decide whether allowing is safe.
func (e Env) Allow(ctx context.Context) error {
	bin := e.DirenvBin
	if bin == "" {
		bin = "direnv"
	}
	return proc.Run(ctx, 2*time.Minute, proc.Spec{
		Name: bin,
		Args: []string{"allow"},
		Dir:  e.Worktree,
	})
}

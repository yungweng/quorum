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
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/proc"
)

// Env is a worktree plus the decision whether direnv wraps commands in it.
type Env struct {
	Worktree string
	Direnv   bool
	// GitConfig keeps agent-run `git config` writes out of the repository's
	// shared config. Other git commands ignore GIT_CONFIG and behave normally.
	GitConfig string
	// GitHooksPath keeps hook installers and agent-run git commands inside the
	// worktree's private hook snapshot instead of the repository's shared hooks.
	GitHooksPath string
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
	env, err := commandEnv(e.GitConfig, e.GitHooksPath)
	if err != nil {
		return err
	}
	return proc.Run(ctx, timeout, proc.Spec{
		Name:   name,
		Args:   args,
		Dir:    e.Worktree,
		Env:    env,
		Stdin:  c.Stdin,
		Stdout: c.Stdout,
		Stderr: c.Stderr,
	})
}

func commandEnv(config, hooksPath string) ([]string, error) {
	env := goCacheEnv()
	if config != "" {
		env = setEnv(env, "GIT_CONFIG", config)
	}
	if hooksPath == "" {
		return env, nil
	}
	count := 0
	if value, ok := envValue(env, "GIT_CONFIG_COUNT"); ok {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 0 {
			return nil, fmt.Errorf("invalid GIT_CONFIG_COUNT %q", value)
		}
	}
	env = setEnv(env, "GIT_CONFIG_COUNT", strconv.Itoa(count+1))
	env = setEnv(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", count), "core.hooksPath")
	env = setEnv(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", count), hooksPath)
	return env, nil
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix), true
		}
	}
	return "", false
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i := range env {
		if strings.HasPrefix(env[i], prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// goCacheEnv makes Go build outputs reusable across quorum's uniquely named
// worktrees. An explicit setting wins because some projects test path handling.
func goCacheEnv() []string {
	env := os.Environ()
	flags := os.Getenv("GOFLAGS")
	for _, flag := range strings.Fields(flags) {
		if flag == "-trimpath" || strings.HasPrefix(flag, "-trimpath=") {
			return env
		}
	}
	value := strings.TrimSpace(flags + " -trimpath")
	for i, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			env[i] = "GOFLAGS=" + value
			return env
		}
	}
	return append(env, "GOFLAGS="+value)
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
	env, err := commandEnv(e.GitConfig, e.GitHooksPath)
	if err != nil {
		return err
	}
	return proc.Run(ctx, 2*time.Minute, proc.Spec{
		Name: bin,
		Args: []string{"allow"},
		Dir:  e.Worktree,
		Env:  env,
	})
}

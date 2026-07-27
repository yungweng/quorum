// Package proc runs external commands with a timeout and kills everything they
// spawned when that timeout hits.
//
// This replaces the run_with_timeout / kill_tree / force_kill_tree trio that
// existed in slightly different forms in both shell tools. Those walked `ps`
// output recursively and killed pid by pid, which races: a child spawned
// between the walk and the kill survives and keeps the run alive. Here the
// command becomes the leader of its own process group, every descendant
// inherits that group, and one kill to the negative pid reaches all of them
// atomically. Codex spawns MCP servers and language toolchains below itself, so
// leaking one of those means leaking whatever it was doing.
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ErrTimeout reports that the command was killed for exceeding its timeout.
// The shell version signalled this as exit 124, which callers still surface.
var ErrTimeout = errors.New("timed out")

// TimeoutExitCode is what the shell tools returned on a timeout, kept because
// the babysit pipeline distinguishes a timeout from an ordinary failure.
const TimeoutExitCode = 124

// graceTime is how long a process group gets to honour SIGTERM before SIGKILL.
// The shell version slept 2 seconds between the two, and Codex needs roughly
// that to flush its rollout file, which is what session recovery reads later.
const graceTime = 2 * time.Second

// Spec describes one command to run.
type Spec struct {
	Name   string
	Args   []string
	Dir    string
	Env    []string // nil inherits the parent environment
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run starts the command and waits for it. A timeout of zero disables the
// timeout, matching `--review-timeout 0` and `--fix-timeout 0`.
//
// On timeout the whole process group gets SIGTERM, then SIGKILL after
// graceTime, and Run returns ErrTimeout. A cancelled ctx does the same but
// returns ctx.Err().
func Run(ctx context.Context, timeout time.Duration, s Spec) error {
	cmd := exec.Command(s.Name, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.Stdin = s.Stdin
	cmd.Stdout = s.Stdout
	cmd.Stderr = s.Stderr
	// Setpgid makes this process the leader of a new group, so a kill to -pid
	// reaches every descendant. It also detaches the command from the
	// terminal's group, which means a Ctrl-C in the terminal no longer reaches
	// the children directly; callers cancel ctx from their signal handler
	// instead, and the kill below does the rest.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", s.Name, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	select {
	case err := <-done:
		return err
	case <-timer:
		killGroup(cmd.Process.Pid)
		<-done // reap, so the child never becomes a zombie
		return ErrTimeout
	case <-ctx.Done():
		killGroup(cmd.Process.Pid)
		<-done
		return ctx.Err()
	}
}

// killGroup terminates the process group led by pid: SIGTERM, a grace period,
// then SIGKILL for whatever ignored it.
func killGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	deadline := time.After(graceTime)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return
		case <-tick.C:
			// Signal 0 probes the group without sending anything. ESRCH means
			// nothing is left, so the SIGKILL can be skipped entirely.
			if err := syscall.Kill(-pid, 0); err != nil {
				return
			}
		}
	}
}

// Alive reports whether a process exists. Signal 0 performs the permission and
// existence checks without delivering anything; EPERM means it is there but
// belongs to somebody else, which still counts as running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ClaimFile is the persistent sentinel and lock file Claim writes into a run
// directory.
const ClaimFile = "run.pid"

// Claim marks a directory as belonging to this process until the returned
// release function is called.
//
// The cache collector has to tell a run that is over from one still using its
// worktree, and no registry can answer that for it: the agent's markers cover
// only the reviews the agent itself started, so a `quorum review` in a terminal
// looked finished from the moment it began and could have its worktree deleted
// underneath it. The claim lives in the directory because the directory is what
// gets collected.
//
// The file lock, rather than the PID written for diagnostics, identifies the
// process instance. The kernel releases it when the process exits, so PID reuse
// cannot make a dead run look live. Release leaves the unlocked file in place
// to keep the inode stable for concurrent readers and to distinguish runs made
// by claim-aware versions from legacy run directories.
func Claim(dir string) (func(), error) {
	path := filepath.Join(dir, ClaimFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("claiming %s: %w", dir, err)
	}
	fail := func(err error) (func(), error) {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	if err := f.Truncate(0); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		return fail(err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}, nil
}

// Claimed reports whether a process still holds a directory's claim lock.
// A missing or unlocked claim counts as unclaimed; callers decide whether a
// compatibility grace still protects a directory written by an older version.
func Claimed(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, ClaimFile), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Failure to take the lock means another process has it. Treat any
		// other lock error conservatively too: deleting an uncertain run is
		// worse than leaving it for the next collection.
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// LockDir creates a directory if needed and takes an exclusive cross-process
// lock on it. Locking the directory itself avoids a persistent lock file.
func LockDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}, nil
}

// ExitCode extracts the exit status from a Run error. A timeout reports
// TimeoutExitCode, matching what the shell tools produced; anything that is
// not an exit status at all reports 1.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, ErrTimeout) {
		return TimeoutExitCode
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

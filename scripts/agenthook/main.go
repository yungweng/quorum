package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var relevantPathspecs = []string{
	":(glob)**/*.go",
	"go.mod",
	"go.sum",
	"go.work",
	"go.work.sum",
	"Makefile",
	".golangci.yml",
	".github/workflows/ci.yml",
	":(glob).githooks/**",
	":(glob).claude/**",
	":(glob).codex/**",
}

type hookInput struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
}

func main() {
	os.Exit(runCommandHook(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func runCommandHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var feedback bytes.Buffer
	code := runHook(args, stdin, &feedback)
	if code == 2 {
		decision := struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}{
			Decision: "block",
			Reason:   feedback.String(),
		}
		if err := json.NewEncoder(stdout).Encode(decision); err != nil {
			fmt.Fprintf(stderr, "encode agent hook decision: %v\n", err)
			return 1
		}
		return 0
	}
	if feedback.Len() > 0 {
		_, _ = stderr.Write(feedback.Bytes())
	}
	return code
}

func runHook(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: agenthook start|stop|end")
		return 1
	}
	event := args[0]
	if event != "start" && event != "stop" && event != "end" {
		fmt.Fprintf(stderr, "unknown agent hook event %q\n", event)
		return 1
	}

	var input hookInput
	if err := json.NewDecoder(stdin).Decode(&input); err != nil {
		fmt.Fprintf(stderr, "decode agent hook input: %v\n", err)
		return 1
	}
	if input.SessionID == "" || input.CWD == "" {
		fmt.Fprintln(stderr, "agent hook input is missing session_id or cwd")
		return 1
	}

	repo, err := gitOutput(input.CWD, "rev-parse", "--show-toplevel")
	if err != nil {
		// Hooks can fire after the agent leaves the repository. There is nothing
		// for this project to check in that case.
		return 0
	}
	repo = strings.TrimSpace(repo)
	stateDir, err := gitOutput(repo, "rev-parse", "--path-format=absolute", "--git-path", "quorum-hook-state")
	if err != nil {
		fmt.Fprintf(stderr, "resolve agent hook state directory: %v\n", err)
		return 1
	}
	stateDir = strings.TrimSpace(stateDir)
	if !filepath.IsAbs(stateDir) {
		stateDir = filepath.Join(repo, stateDir)
	}

	key := sha256.Sum256([]byte(input.SessionID))
	basePath := filepath.Join(stateDir, hex.EncodeToString(key[:])+".base")
	failedPath := filepath.Join(stateDir, hex.EncodeToString(key[:])+".failed")

	switch event {
	case "start":
		if _, err := os.Stat(basePath); err == nil {
			return 0
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "read agent hook state: %v\n", err)
			return 1
		}
		fingerprint, err := worktreeFingerprint(repo)
		if err != nil {
			fmt.Fprintf(stderr, "fingerprint working tree: %v\n", err)
			return 1
		}
		if err := writeState(basePath, fingerprint); err != nil {
			fmt.Fprintf(stderr, "write agent hook state: %v\n", err)
			return 1
		}
		_ = os.Remove(failedPath)
		return 0

	case "end":
		_ = os.Remove(basePath)
		_ = os.Remove(failedPath)
		return 0
	}

	fingerprint, err := worktreeFingerprint(repo)
	if err != nil {
		fmt.Fprintf(stderr, "fingerprint working tree: %v\n", err)
		return 1
	}
	base, err := os.ReadFile(basePath)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeState(basePath, fingerprint); err != nil {
			fmt.Fprintf(stderr, "write agent hook state: %v\n", err)
			return 1
		}
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "read agent hook state: %v\n", err)
		return 1
	}
	if bytes.Equal(base, fingerprint) {
		return 0
	}

	if input.StopHookActive {
		failed, err := os.ReadFile(failedPath)
		if err == nil && bytes.Equal(failed, fingerprint) {
			fmt.Fprintln(stderr, "Quality checks still fail and no relevant files changed; allowing this stop to avoid a hook loop.")
			return 0
		}
	}

	output, checkErr := runCheck(repo)
	if checkErr != nil {
		if err := writeState(failedPath, fingerprint); err != nil {
			fmt.Fprintf(stderr, "write failed agent hook state: %v\n", err)
			return 1
		}
		fmt.Fprintln(stderr, "Quality checks failed after this session changed project code:")
		if len(output) > 0 {
			_, _ = stderr.Write(output)
			if output[len(output)-1] != '\n' {
				fmt.Fprintln(stderr)
			}
		}
		fmt.Fprintln(stderr, "Fix the failure and run make check before stopping.")
		return 2
	}

	if err := writeState(basePath, fingerprint); err != nil {
		fmt.Fprintf(stderr, "update agent hook state: %v\n", err)
		return 1
	}
	_ = os.Remove(failedPath)
	return 0
}

func worktreeFingerprint(repo string) ([]byte, error) {
	args := []string{"ls-files", "--cached", "--others", "--deduplicate", "-z", "--"}
	args = append(args, relevantPathspecs...)
	listed, err := gitBytes(repo, args...)
	if err != nil {
		return nil, err
	}

	hash := sha256.New()
	scanner := bufio.NewScanner(bytes.NewReader(listed))
	scanner.Split(splitNUL)
	for scanner.Scan() {
		path := scanner.Text()
		if path == "" {
			continue
		}
		fullPath := filepath.Join(repo, filepath.FromSlash(path))
		_, _ = io.WriteString(hash, path)
		_, _ = hash.Write([]byte{0})
		info, statErr := os.Lstat(fullPath)
		if errors.Is(statErr, os.ErrNotExist) {
			_, _ = io.WriteString(hash, "missing\x00")
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		_, _ = io.WriteString(hash, strconv.FormatUint(uint64(info.Mode()), 8))
		_, _ = hash.Write([]byte{0})
		var contents []byte
		switch {
		case info.Mode().IsRegular():
			contents, err = os.ReadFile(fullPath)
		case info.Mode()&os.ModeSymlink != 0:
			var target string
			target, err = os.Readlink(fullPath)
			contents = []byte(target)
		default:
			return nil, fmt.Errorf("fingerprint %s: unsupported file type %s", path, info.Mode().Type())
		}
		if err != nil {
			return nil, err
		}
		fileHash := sha256.Sum256(contents)
		_, _ = hash.Write(fileHash[:])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}

func splitNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func writeState(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func runCheck(repo string) ([]byte, error) {
	cmd := exec.Command("make", "check")
	cmd.Dir = repo
	cmd.Env = withoutEnv("GO111MODULE")
	return cmd.CombinedOutput()
}

func withoutEnv(name string) []string {
	prefix := name + "="
	env := os.Environ()
	filtered := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func gitOutput(repo string, args ...string) (string, error) {
	output, err := gitBytes(repo, args...)
	return string(output), err
}

func gitBytes(repo string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

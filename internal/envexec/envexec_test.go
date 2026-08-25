package envexec

import (
	"bytes"
	"context"
	"testing"
)

func TestRunAddsTrimpathToExistingGoFlags(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=readonly")
	var out bytes.Buffer
	err := (Env{Worktree: t.TempDir()}).Run(context.Background(), 0, Cmd{
		Name: "/bin/sh", Args: []string{"-c", `printf %s "$GOFLAGS"`}, Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "-mod=readonly -trimpath"; got != want {
		t.Fatalf("GOFLAGS = %q, want %q", got, want)
	}
}

func TestRunRespectsExplicitTrimpathSetting(t *testing.T) {
	t.Setenv("GOFLAGS", "-trimpath=false")
	var out bytes.Buffer
	err := (Env{Worktree: t.TempDir()}).Run(context.Background(), 0, Cmd{
		Name: "/bin/sh", Args: []string{"-c", `printf %s "$GOFLAGS"`}, Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "-trimpath=false"; got != want {
		t.Fatalf("GOFLAGS = %q, want %q", got, want)
	}
}

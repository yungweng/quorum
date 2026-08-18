package notify

import (
	"reflect"
	"testing"
)

func TestPersistentCommandCarriesSound(t *testing.T) {
	cmd := command(
		"quorum: approval required",
		"acme/api#42 is clean. Ask another reviewer to approve it.",
		true,
	)
	want := []string{
		"/usr/bin/osascript",
		"-e", "on run argv",
		"-e", `display notification (item 2 of argv) with title (item 1 of argv) sound name "default"`,
		"-e", "end run",
		"quorum: approval required",
		"acme/api#42 is clean. Ask another reviewer to approve it.",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
}

func TestRoutineCommandIsSilent(t *testing.T) {
	cmd := command("Reviewed acme/api#42", "nothing found", false)
	want := []string{
		"/usr/bin/osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"Reviewed acme/api#42",
		"nothing found",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
}

func TestCommandPassesTextAsArgumentsUnquoted(t *testing.T) {
	title, body := `he said "hi"`, `back\slash and 'quotes'`
	cmd := command(title, body, false)
	if got := cmd.Args[len(cmd.Args)-2]; got != title {
		t.Fatalf("title argument = %q, want %q", got, title)
	}
	if got := cmd.Args[len(cmd.Args)-1]; got != body {
		t.Fatalf("body argument = %q, want %q", got, body)
	}
}

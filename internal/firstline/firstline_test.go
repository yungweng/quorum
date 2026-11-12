package firstline

import "testing"

func TestTrimmed(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"  fatal: nope  ":  "fatal: nope",
		"one  \ntwo":       "one",
		"\nafter a blank":  "",
		"error\r\ndetails": "error",
	} {
		if got := Trimmed(in); got != want {
			t.Errorf("Trimmed(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrNoOutput(t *testing.T) {
	for in, want := range map[string]string{
		"":                "no output",
		" \n\t":           "no output",
		"one  \ntwo":      "one  ",
		"\nafter a blank": "after a blank",
		"  single  ":      "single",
	} {
		if got := OrNoOutput(in); got != want {
			t.Errorf("OrNoOutput(%q) = %q, want %q", in, got, want)
		}
	}
}

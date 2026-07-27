package runner

import (
	"os/exec"
	"runtime"
	"strings"
)

// notify posts a desktop notification. When terminal-notifier is installed the
// notification carries the URL, so clicking it opens the posted review comment;
// osascript cannot do that, which is why it is only the fallback.
func (r *Runner) notify(title, body, url string) {
	if !r.Cfg.Notify || runtime.GOOS != "darwin" {
		return
	}
	if bin, err := exec.LookPath("terminal-notifier"); err == nil {
		args := []string{"-title", title, "-message", body, "-group", "io.github.quorum"}
		if url != "" {
			args = append(args, "-open", url)
		}
		if err := exec.Command(bin, args...).Run(); err == nil {
			return
		}
	}
	script := "display notification " + quote(body) + " with title " + quote(title)
	exec.Command("osascript", "-e", script).Run()
}

// quote produces an AppleScript string literal.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

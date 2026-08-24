// Command quorum reviews pull requests with a panel of independent Codex
// reviewers, drives them to green, and can do both unattended.
package main

import (
	"os"

	"github.com/yungweng/quorum/internal/cli"
)

// Version is the release. The Homebrew formula builds it in with
// -ldflags "-X main.Version=...", and the release job in ci.yml reads this
// line out of this file with sed, so both the name and the shape of this
// declaration are load-bearing. Keep it a plain `var Version = "X.Y.Z"` here
// in package main.
var Version = "1.14.3"

func main() {
	os.Exit(cli.Run(Version, os.Args[1:]))
}

package agentstate

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// WriteHeartbeat records that a poll completed and how many open pull requests
// it saw. The heartbeat is what tells "the agent is loaded" from "the agent is
// actually working". It is best effort: a missed heartbeat only makes the
// agent look idle until the next poll.
func WriteHeartbeat(path string, open int) {
	os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+" "+strconv.Itoa(open)+"\n"), 0o644)
}

// ReadHeartbeat returns when the last poll completed and how many open pull
// requests it saw. ok is false when no readable heartbeat exists.
func ReadHeartbeat(path string) (at time.Time, open int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, 0, false
	}
	parts := strings.Fields(string(b))
	if len(parts) == 0 {
		return time.Time{}, 0, false
	}
	at, err = time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return time.Time{}, 0, false
	}
	if len(parts) > 1 {
		open, _ = strconv.Atoi(parts[1])
	}
	return at, open, true
}

package cli

import (
	"fmt"

	"github.com/yungweng/quorum/internal/automerge"
)

// queuePosition is the " · position 3" suffix for a queued result. GitHub does
// not always report a position, and a missing one must not print as zero.
func queuePosition(result automerge.Result) string {
	if result.Status != automerge.Queued || result.QueuePosition <= 0 {
		return ""
	}
	return fmt.Sprintf(" · position %d", result.QueuePosition)
}

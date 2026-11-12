package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/yungweng/quorum/internal/atomicfile"
	"github.com/yungweng/quorum/internal/gh"
)

const endStateCacheSchema = 1

type endStateCacheFile struct {
	Schema int               `json:"schema"`
	States map[string]string `json:"states"`
}

// readEndStateCache loads the dashboard's last known GitHub states. The cache
// is optional and disposable, so a missing, truncated or future-version file
// is the same as an empty one. Invalid values are dropped instead of reaching
// the renderer as facts about a pull request.
func readEndStateCache(path string) map[string]string {
	states := map[string]string{}
	if path == "" {
		return states
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return states
	}
	var file endStateCacheFile
	if json.Unmarshal(b, &file) != nil || file.Schema != endStateCacheSchema {
		return states
	}
	for key, state := range file.States {
		if gh.ValidPRState(state) {
			states[key] = state
		}
	}
	return states
}

// writeEndStateCache atomically replaces the small cache. A process-specific
// temporary name keeps two dashboard processes from writing through the same
// file; the last complete snapshot wins, and either can rebuild it.
func writeEndStateCache(path string, states map[string]string) error {
	if path == "" {
		return nil
	}
	clean := make(map[string]string, len(states))
	for key, state := range states {
		if gh.ValidPRState(state) {
			clean[key] = state
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteJSON(path, endStateCacheFile{Schema: endStateCacheSchema, States: clean})
}

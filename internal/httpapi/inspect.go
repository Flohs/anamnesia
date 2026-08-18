// inspect.go serves the two things about an install that are not in the
// database: the resolved configuration, and the hook log.
//
// Both live on the host side. Settings are declared in package main,
// which this package cannot import, so serve.go resolves and masks them
// and hands the result over as data. The hook log is a file, so this
// only needs its path.
package httpapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"time"
)

// ConfigItem is one resolved setting. Value is already masked for
// secrets by the time it reaches here: the HTTP layer never sees a key.
type ConfigItem struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"` // default | global | project | env | flag
	Secret bool   `json:"secret"`
}

// HookLogEntry is one line of ~/.anamnesia/hooks.log. The shape is the
// one the hooks write; it is duplicated here rather than shared because
// the writer lives in package main and `doctor` reads it through its own
// path, which this must not disturb.
type HookLogEntry struct {
	At    time.Time `json:"at"`
	Verb  string    `json:"verb"`
	OK    bool      `json:"ok"`
	Ms    int64     `json:"ms"`
	Note  string    `json:"note,omitempty"`
	Error string    `json:"error,omitempty"`
}

// hookLogTail is how many entries /v1/hooks returns at most.
const hookLogTail = 200

func (d Deps) handleConfig(w http.ResponseWriter, r *http.Request) {
	items := d.Config
	if items == nil {
		items = []ConfigItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleHooks serves the parsed tail of the hook log, newest first.
//
// This is the only history that survives a restart, which makes it worth
// more than its size suggests: it is the difference between "memory is
// working" and "every hook has failed silently since Tuesday".
func (d Deps) handleHooks(w http.ResponseWriter, r *http.Request) {
	if d.HookLogPath == "" {
		http.Error(w, "no hook log path is configured", http.StatusNotFound)
		return
	}
	items, err := readHookLog(d.HookLogPath, hookLogTail)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": d.HookLogPath, "items": items})
}

// readHookLog returns up to n entries, newest first. A log that does not
// exist yet means no hook has ever run, which is an empty list and not
// an error: it is one of the states the UI exists to explain.
func readHookLog(path string, n int) ([]HookLogEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []HookLogEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()

	entries := make([]HookLogEntry, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e HookLogEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // a truncated line from a rotation is not a failure
		}
		entries = append(entries, e)
		if len(entries) > n {
			entries = entries[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Newest first, matching every other list this API serves.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

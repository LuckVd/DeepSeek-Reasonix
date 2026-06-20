package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"reasonix/internal/fileutil"
)

const (
	cacheSuffix       = ".snapshot.json"
	staleMessageDelta = 6         // background pre-gen batches: only refresh once this many new messages land
	staleMaxAge       = time.Hour // or once this long has passed
	fullResyncEvery   = 8         // after this many incremental updates, do a full re-summarization to correct drift
)

// cacheEntry is the sidecar record at <sessionPath>.snapshot.json. It holds the
// rolling snapshot plus a CoveredCount cursor (how many messages the snapshot
// summarizes) and an Updates counter (incremental refreshes since the last full
// re-summarization). The cursor is what makes updates incremental: a refresh
// feeds the model the previous snapshot + only the messages past the cursor, so
// a long task costs ~constant tokens per refresh instead of growing with the
// transcript.
type cacheEntry struct {
	Snapshot     Snapshot `json:"snapshot"`
	CoveredCount int      `json:"coveredCount"` // len(messages) the snapshot summarizes
	GeneratedAt  int64    `json:"generatedAt"`  // unix seconds
	Updates      int      `json:"updates"`      // incremental updates since the last full re-summarization
}

func cachePath(sessionPath string) string {
	if sessionPath == "" {
		return ""
	}
	return sessionPath + cacheSuffix
}

func loadEntry(sessionPath string) *cacheEntry {
	p := cachePath(sessionPath)
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	return &entry
}

func agedOut(generatedAt int64) bool {
	return time.Now().Unix()-generatedAt > int64(staleMaxAge.Seconds())
}

// Load returns the last cached snapshot for the session regardless of freshness,
// or nil if there is none. Used for stale-while-revalidate: an expand shows the
// last summary instantly while a background refresh updates it. Cheap sidecar
// read; never calls the provider.
func Load(sessionPath string) *Snapshot {
	if e := loadEntry(sessionPath); e != nil {
		snap := e.Snapshot
		return &snap
	}
	return nil
}

// NeedsUpdate reports whether a background refresh would do real work: no cached
// snapshot yet, at least staleMessageDelta new messages since the last one, or
// the cache is past staleMaxAge. The board calls this at stop points to pre-warm
// the cache without firing on every refresh. Cheap — a sidecar read only, no
// provider, no LLM.
func NeedsUpdate(sessionPath string, msgCount int) bool {
	e := loadEntry(sessionPath)
	if e == nil {
		return true
	}
	if msgCount-e.CoveredCount >= staleMessageDelta {
		return true
	}
	if agedOut(e.GeneratedAt) {
		return true
	}
	return false
}

// saveEntry writes the rolling snapshot atomically (tmp + rename), mirroring
// branch.go's sidecar pattern. Errors are swallowed — caching is best-effort; a
// missed write just means the next view regenerates.
func saveEntry(sessionPath string, coveredCount int, snap *Snapshot, updates int) {
	p := cachePath(sessionPath)
	if p == "" || snap == nil {
		return
	}
	entry := cacheEntry{Snapshot: *snap, CoveredCount: coveredCount, GeneratedAt: snap.GeneratedAt, Updates: updates}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".snapshot.*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return
	}
	_ = fileutil.ReplaceFile(tmpPath, p)
}

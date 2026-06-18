package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"reasonix/internal/fileutil"
	"reasonix/internal/provider"
)

const (
	cacheSuffix       = ".snapshot.json"
	staleMessageDelta = 6         // regenerate once this many new messages land
	staleMaxAge       = time.Hour // or once this long has passed
)

// cacheEntry is the sidecar record at <sessionPath>.snapshot.json.
type cacheEntry struct {
	MessageCount int      `json:"messageCount"` // len(messages) when generated
	GeneratedAt  int64    `json:"generatedAt"`  // unix seconds
	Snapshot     Snapshot `json:"snapshot"`
}

func cachePath(sessionPath string) string {
	if sessionPath == "" {
		return ""
	}
	return sessionPath + cacheSuffix
}

// loadCache returns a still-fresh snapshot, or (nil, false). A snapshot goes
// stale once staleMessageDelta new messages arrive or staleMaxAge elapses — so a
// task viewed repeatedly costs at most one call per chunk of new activity.
func loadCache(sessionPath string, msgs []provider.Message) (*Snapshot, bool) {
	p := cachePath(sessionPath)
	if p == "" {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	if abs(len(msgs)-entry.MessageCount) >= staleMessageDelta {
		return nil, false
	}
	if time.Now().Unix()-entry.GeneratedAt > int64(staleMaxAge.Seconds()) {
		return nil, false
	}
	snap := entry.Snapshot
	return &snap, true
}

// saveCache writes the snapshot atomically (tmp + rename), mirroring branch.go's
// sidecar pattern. Errors are swallowed — caching is best-effort; a missed write
// just means the next view regenerates.
func saveCache(sessionPath string, msgs []provider.Message, snap *Snapshot) {
	p := cachePath(sessionPath)
	if p == "" || snap == nil {
		return
	}
	entry := cacheEntry{MessageCount: len(msgs), GeneratedAt: snap.GeneratedAt, Snapshot: *snap}
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

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

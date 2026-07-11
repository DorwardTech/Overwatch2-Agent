// Package buffer is a bounded FIFO of unsent payloads. When central is
// unreachable, batches are queued here and replayed oldest-first on recovery;
// when full, the oldest batch is dropped (recent telemetry matters most).
// The queue lives in memory; Save/Load spill it to disk across restarts so an
// outage that overlaps a restart (redeploy, reboot_agent) doesn't silently
// drop buffered telemetry.
package buffer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Entry is a queued payload with its idempotency key.
type Entry struct {
	Key  string
	Data []byte
}

type Buffer struct {
	mu    sync.Mutex
	items []Entry
	max   int
}

func New(max int) *Buffer {
	if max < 1 {
		max = 1
	}
	return &Buffer{max: max}
}

// Push appends an entry, dropping the oldest if at capacity.
func (b *Buffer) Push(key string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) >= b.max {
		b.items = b.items[1:]
	}
	b.items = append(b.items, Entry{Key: key, Data: data})
}

// Peek returns the oldest entry without removing it.
func (b *Buffer) Peek() (Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return Entry{}, false
	}
	return b.items[0], true
}

// PopFront removes the oldest entry (after a successful send).
func (b *Buffer) PopFront() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) > 0 {
		b.items = b.items[1:]
	}
}

func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// Save writes the queued entries to path (atomic: temp file + rename). An
// empty buffer removes the file so a later Load starts clean.
func (b *Buffer) Save(path string) error {
	b.mu.Lock()
	items := make([]Entry, len(b.items))
	copy(items, b.items)
	b.mu.Unlock()

	if len(items) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load restores previously-saved entries (oldest-first, capacity-bounded) and
// removes the spill file. Returns the number of entries restored; a missing
// file is not an error.
func (b *Buffer) Load(path string) (int, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var items []Entry
	if err := json.Unmarshal(data, &items); err != nil {
		_ = os.Remove(path) // corrupt spill: drop it rather than crash-loop
		return 0, err
	}

	b.mu.Lock()
	if len(items) > b.max {
		items = items[len(items)-b.max:] // keep the most recent
	}
	b.items = append(items, b.items...)
	b.mu.Unlock()

	_ = os.Remove(path)
	return len(items), nil
}

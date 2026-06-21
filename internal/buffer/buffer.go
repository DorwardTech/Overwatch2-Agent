// Package buffer is a bounded in-memory FIFO of unsent payloads. When central is
// unreachable, batches are queued here and replayed oldest-first on recovery;
// when full, the oldest batch is dropped (recent telemetry matters most).
package buffer

import "sync"

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

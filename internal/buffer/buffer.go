// Package buffer is a bounded FIFO of unsent payloads. When central is
// unreachable, batches are queued here and replayed oldest-first on recovery;
// when full, the oldest batch is dropped (recent telemetry matters most).
// The queue lives in memory; Save/Load spill it to disk across restarts so an
// outage that overlaps a restart (redeploy, reboot_agent) doesn't silently
// drop buffered telemetry.
//
// The queue is bounded twice: by entry count, and by total bytes. Count alone
// is not a memory bound, because it says nothing about how large an entry is —
// and the pack-IR listener accepts entries from anywhere on the venue LAN
// without authentication. At the default 2000 entries and its 64 KiB body cap,
// count alone permits 128 MB of queue inside a 128 MB container: an OOM kill,
// which skips the graceful spill and takes the unsent telemetry with it.
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

	// id is process-local identity, assigned by Push and deliberately
	// unexported so it stays out of the spill file's JSON — it identifies a
	// slot in this queue, not the batch, and means nothing across a restart.
	// PopSent uses it to prove the head is still the entry that was handed out
	// by Peek. Load assigns ids to restored entries so none is ever left at
	// the zero value, which therefore cannot match anything in the queue.
	id uint64
}

// MaxBytes caps the total payload the queue will hold, whatever the entry
// count allows. 32 MB is generous for the telemetry path (a batch is a few KB,
// so thousands still fit) while leaving the 128 MB container ample headroom.
const MaxBytes = 32 << 20

type Buffer struct {
	mu       sync.Mutex
	items    []Entry
	bytes    int    // total len(Data) currently queued
	max      int    // entry-count bound
	maxBytes int    // payload-size bound
	nextID   uint64 // monotonic source of Entry.id
}

func New(max int) *Buffer {
	if max < 1 {
		max = 1
	}
	return &Buffer{max: max, maxBytes: MaxBytes}
}

// Push appends an entry, dropping the oldest until the queue is inside both
// the entry-count and the byte budget. A single entry larger than the whole
// budget is still kept — dropping it outright would silently lose data, and it
// is the only entry left by the time the loop is done.
func (b *Buffer) Push(key string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	b.items = append(b.items, Entry{Key: key, Data: data, id: b.nextID})
	b.bytes += len(data)
	b.trimLocked()
}

// trimLocked drops oldest entries until the queue fits both bounds.
func (b *Buffer) trimLocked() {
	for len(b.items) > 1 && (len(b.items) > b.max || b.bytes > b.maxBytes) {
		b.bytes -= len(b.items[0].Data)
		b.items = b.items[1:]
	}
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

// PopSent removes the head only if it is still the entry Peek handed out, and
// reports whether it did.
//
// This is the whole gap between Peek and the pop. A caller peeks the head,
// spends an HTTPS round trip sending it, and pops it on success — but the
// queue is bounded and drops OLDEST-first, so a Push arriving during that
// round trip can evict the very entry being sent. An unconditional PopFront
// then removes whatever took its place, which has NOT been sent: a batch of
// telemetry lost silently, and lost precisely when the buffer is under enough
// pressure to be evicting, which is when it matters most.
//
// Returning false is not an error. It means the entry was already dropped by
// capacity, so there is nothing left to remove and the caller should simply
// carry on to the new head.
func (b *Buffer) PopSent(sent Entry) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.items) == 0 || b.items[0].id != sent.id {
		return false
	}

	b.bytes -= len(b.items[0].Data)
	b.items = b.items[1:]

	return true
}

// PopFront removes the oldest entry (after a successful send).
func (b *Buffer) PopFront() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) > 0 {
		b.bytes -= len(b.items[0].Data)
		b.items = b.items[1:]
	}
}

func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// Bytes returns the total payload currently queued.
func (b *Buffer) Bytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes
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
	queued := len(b.items)

	// Entry.id does not survive JSON, so restored entries arrive at the zero
	// value. Give them fresh ids before they join the queue: PopSent compares
	// on id, and a queue full of zeroes would let it pop an entry that was
	// never sent. Only the restored slice is numbered — renumbering entries
	// already queued would invalidate an id a concurrent drain is holding, and
	// the resulting PopSent miss would re-send a batch rather than drop one.
	for i := range items {
		b.nextID++
		items[i].id = b.nextID
	}

	b.items = append(items, b.items...)
	b.bytes = 0
	for _, e := range b.items {
		b.bytes += len(e.Data)
	}
	b.trimLocked() // restored entries are bounded exactly like pushed ones
	restored := max(len(b.items)-queued, 0)
	b.mu.Unlock()

	_ = os.Remove(path)
	return restored, nil
}

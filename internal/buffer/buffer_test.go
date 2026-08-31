package buffer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.json")

	b := New(10)
	b.Push("1", []byte(`{"push_seq":1}`))
	b.Push("2", []byte(`{"push_seq":2}`))
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}

	restored := New(10)
	n, err := restored.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || restored.Len() != 2 {
		t.Fatalf("restored %d entries (len %d), want 2", n, restored.Len())
	}
	first, _ := restored.Peek()
	if first.Key != "1" || string(first.Data) != `{"push_seq":1}` {
		t.Fatalf("oldest-first order lost: got key %q data %q", first.Key, first.Data)
	}
	// The spill file is consumed on load — a second load restores nothing.
	if n, _ := New(10).Load(path); n != 0 {
		t.Fatalf("spill file should be removed after load, restored %d", n)
	}
}

func TestLoadKeepsMostRecentWhenOverCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.json")

	b := New(10)
	for i := 0; i < 5; i++ {
		b.Push(string(rune('a'+i)), []byte{byte(i)})
	}
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}

	small := New(2)
	if n, _ := small.Load(path); n != 2 {
		t.Fatalf("restored %d, want capacity-bounded 2", n)
	}
	first, _ := small.Peek()
	if first.Key != "d" {
		t.Fatalf("should keep the most recent entries, oldest kept = %q", first.Key)
	}
}

func TestSaveEmptyRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.json")
	b := New(10)
	b.Push("1", []byte("x"))
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	b.PopFront()
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an empty buffer should remove the spill file")
	}
}

func TestLoadToleratesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(10).Load(path); err == nil {
		t.Fatal("corrupt spill should report an error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("corrupt spill file should be removed so the agent doesn't crash-loop")
	}
}

// Entry count alone is not a memory bound: the pack-IR listener takes entries
// from anywhere on the venue LAN, and at 2000 entries of 64 KiB the queue would
// be the whole 128 MB container. The byte budget is what actually bounds it.
func TestPushHonoursTheByteBudget(t *testing.T) {
	b := New(10_000) // an entry cap far too high to be the binding one
	b.maxBytes = 4096

	for i := 0; i < 64; i++ { // 64 KiB pushed, sixteen times the budget
		b.Push("k", make([]byte, 1024))
	}

	if got := b.Bytes(); got > b.maxBytes {
		t.Fatalf("queued %d bytes, want at most %d", got, b.maxBytes)
	}
	if b.Len() == 0 {
		t.Fatal("the queue dropped everything; it should keep the most recent")
	}
}

// The production default has to be the one that actually applies.
func TestNewUsesTheDefaultByteBudget(t *testing.T) {
	if got := New(10).maxBytes; got != MaxBytes {
		t.Fatalf("maxBytes = %d, want the package default %d", got, MaxBytes)
	}
}

// A single entry bigger than the whole budget is still kept — dropping it would
// silently lose data, and there is nothing older left to drop instead.
func TestPushKeepsAnOversizedEntry(t *testing.T) {
	b := New(10)
	b.maxBytes = 1024
	b.Push("huge", make([]byte, 4096))

	if b.Len() != 1 {
		t.Fatalf("Len = %d, want the oversized entry kept", b.Len())
	}
}

// The byte count has to track removals too, or the budget leaks away.
func TestPopFrontReleasesBytes(t *testing.T) {
	b := New(10)
	b.Push("a", make([]byte, 100))
	b.Push("b", make([]byte, 250))

	b.PopFront()

	if got := b.Bytes(); got != 250 {
		t.Fatalf("Bytes = %d after popping a 100-byte entry, want 250", got)
	}
	b.PopFront()
	if got := b.Bytes(); got != 0 {
		t.Fatalf("Bytes = %d with an empty queue, want 0", got)
	}
}

// A spill file written before the budget existed (or by a build with a larger
// one) must not put the process straight back over it on restart.
func TestLoadAppliesTheByteBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spill.json")

	items := make([]Entry, 0, 40)
	for i := 0; i < 40; i++ {
		items = append(items, Entry{Key: "k", Data: make([]byte, 1024)})
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	b := New(10_000)
	b.maxBytes = 4096
	n, err := b.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != b.Len() {
		t.Fatalf("reported %d restored but holds %d", n, b.Len())
	}
	if got := b.Bytes(); got > b.maxBytes {
		t.Fatalf("restored %d bytes, want at most %d", got, b.maxBytes)
	}
}

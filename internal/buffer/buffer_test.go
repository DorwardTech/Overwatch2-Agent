package buffer

import (
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

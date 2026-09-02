package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRotatesBySizeAndKeepsGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "agent.log")
	w, err := Open(path, 100, 2)
	if err != nil {
		t.Fatalf("Open (should create the parent directory): %v", err)
	}
	defer w.Close()

	line := func(c byte) string { return strings.Repeat(string(c), 59) + "\n" }
	for _, c := range []byte("ABCD") {
		if _, err := w.Write([]byte(line(c))); err != nil {
			t.Fatalf("write %c: %v", c, err)
		}
	}
	// A: 60 bytes, fits. B: would reach 120 > 100, rotate → .1=A, log=B.
	// C: rotate → .2=A, .1=B, log=C. D: rotate → A dropped, .2=B, .1=C, log=D.
	if got := read(t, path); got != line('D') {
		t.Errorf("current file holds %q, want the D line", got[:1])
	}
	if got := read(t, path+".1"); got != line('C') {
		t.Errorf(".1 holds %q, want the C line", got[:1])
	}
	if got := read(t, path+".2"); got != line('B') {
		t.Errorf(".2 holds %q, want the B line", got[:1])
	}
	if exists(path + ".3") {
		t.Errorf(".3 exists; keep=2 must drop the oldest generation")
	}
}

func TestReopenContinuesCountingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w, err := Open(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("a", 60)
	if _, err := w.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("write after Close succeeded")
	}

	// A restart reopens the same file: the size already on disk must count
	// towards the limit, or the file could grow without bound across restarts.
	w, err = Open(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	second := strings.Repeat("b", 60)
	if _, err := w.Write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path+".1"); got != first {
		t.Errorf("rotated file holds %q, want the pre-restart content", got)
	}
	if got := read(t, path); got != second {
		t.Errorf("current file holds %q, want the post-restart content", got)
	}
}

func TestOversizedWriteLandsWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w, err := Open(path, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	big := strings.Repeat("z", 50)
	if _, err := w.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != big {
		t.Errorf("oversized line was not written whole: %d bytes", len(got))
	}
}

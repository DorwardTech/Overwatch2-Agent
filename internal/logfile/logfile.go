// Package logfile is the agent's log destination when it runs outside a
// container. In a container the log is stdout and the runtime keeps it. A
// Windows service has no stdout, so the agent writes a file under its data
// directory and rotates it by size, keeping a few generations: the log can
// neither vanish nor fill the disk.
package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

const (
	// DefaultMaxBytes is the size at which the current file is rotated.
	DefaultMaxBytes = 10 << 20
	// DefaultKeep is how many rotated generations are kept: agent.log.1 is the
	// newest, agent.log.5 the oldest; the next rotation drops it.
	DefaultKeep = 5
)

// Writer is a size-rotating, append-only log file. It is safe for concurrent
// use, which the standard logger relies on.
type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
	// retryAt defers the next rotation attempt after one fails (a rename
	// refused by a virus scanner holding the file, say), so every write does
	// not pay for another failed attempt.
	retryAt int64
}

// Open opens the log at path for appending, creating the parent directory and
// the file as needed. maxBytes <= 0 and keep < 1 select the defaults.
func Open(path string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep < 1 {
		keep = DefaultKeep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &Writer{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

// Write appends p, rotating first when it would carry the file past the size
// limit. A single write larger than the limit still lands whole.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes && w.size >= w.retryAt {
		if err := w.rotate(); err != nil {
			// Keep the current file rather than lose lines; try again once it
			// has grown by a further eighth of the limit.
			w.retryAt = w.size + w.maxBytes/8
		}
		if w.f == nil {
			return 0, fmt.Errorf("logfile: reopen %s after rotation failed", w.path)
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate closes the current file, shifts the generations (dropping the
// oldest) and reopens a fresh file. The file is closed before the rename
// because Windows will not rename an open file. If the shift fails the
// current file is reopened and stays in use.
func (w *Writer) rotate() error {
	_ = w.f.Close()
	w.f = nil
	shiftErr := w.shift()
	if err := w.open(); err != nil {
		return err
	}
	w.retryAt = 0
	return shiftErr
}

func (w *Writer) shift() error {
	if err := os.Remove(w.generation(w.keep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for i := w.keep - 1; i >= 1; i-- {
		if err := os.Rename(w.generation(i), w.generation(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.Rename(w.path, w.generation(1))
}

func (w *Writer) generation(i int) string { return fmt.Sprintf("%s.%d", w.path, i) }

// Close closes the file. Further writes fail with os.ErrClosed.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

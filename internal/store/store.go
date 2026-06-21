// Package store is the agent's local, verbatim cache of O-Zone print-server game
// data. It persists each game's full "all" payload byte-for-byte on disk so the
// proxy can replay exactly what O-Zone returned (the 1:1 fidelity requirement),
// plus per-game metadata used to rebuild the "list" response.
//
// It is filesystem-backed (one raw file + one metadata file per game) so it needs
// no database, embeds nothing, survives restarts, and keeps the cached bytes
// trivially inspectable. The working set is small — a single venue's recent games,
// pruned to a retention window — so an in-memory metadata index is ample.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// GameMeta is the denormalised summary used to rebuild the O-Zone game list and
// to drive retention. The verbatim payload itself lives alongside as <n>.bin.
type GameMeta struct {
	GameNumber  int       `json:"game_number"`
	GameName    string    `json:"game_name"`
	GameType    int       `json:"game_type"`
	Duration    int       `json:"duration"`
	StartTime   string    `json:"start_time"`
	EndTime     string    `json:"end_time"` // "" is rendered as O-Zone's literal "None"
	PlayerCount int       `json:"player_count"`
	Valid       int       `json:"valid"`
	HasRaw      bool      `json:"has_raw"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store is a concurrency-safe filesystem cache of games.
type Store struct {
	dir  string
	mu   sync.RWMutex
	meta map[int]GameMeta
	now  func() time.Time
}

// Open prepares the cache directory and loads the metadata index from disk.
func Open(dir string) (*Store, error) {
	for _, sub := range []string{"games", "meta"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", sub, err)
		}
	}
	s := &Store{dir: dir, meta: map[int]GameMeta{}, now: time.Now}
	if err := s.loadMeta(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadMeta() error {
	metaDir := filepath.Join(s.dir, "meta")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		return fmt.Errorf("store: read meta dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(metaDir, e.Name()))
		if err != nil {
			continue
		}
		var m GameMeta
		if err := json.Unmarshal(b, &m); err != nil || m.GameNumber <= 0 {
			continue
		}
		s.meta[m.GameNumber] = m
	}
	return nil
}

// UpsertListEntry merges metadata learned from a "list" response. It preserves
// whatever full payload (HasRaw) and game type were already known.
func (s *Store) UpsertListEntry(e GameMeta) error {
	if e.GameNumber <= 0 {
		return errors.New("store: invalid game number")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	m := s.meta[e.GameNumber]
	m.GameNumber = e.GameNumber
	if e.GameName != "" {
		m.GameName = e.GameName
	}
	m.Duration = e.Duration
	m.StartTime = e.StartTime
	m.EndTime = e.EndTime
	m.PlayerCount = e.PlayerCount
	m.Valid = e.Valid
	m.UpdatedAt = s.now()
	return s.saveMetaLocked(m)
}

// StoreGame writes the verbatim "all" payload and merges the metadata derived
// from it, preserving the end time / duration that only the list response knows.
func (s *Store) StoreGame(partial GameMeta, raw []byte) error {
	if partial.GameNumber <= 0 {
		return errors.New("store: invalid game number")
	}
	if len(raw) == 0 {
		return errors.New("store: empty payload")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeRawLocked(partial.GameNumber, raw); err != nil {
		return err
	}

	m := s.meta[partial.GameNumber]
	m.GameNumber = partial.GameNumber
	if partial.GameName != "" {
		m.GameName = partial.GameName
	}
	if partial.GameType != 0 {
		m.GameType = partial.GameType
	}
	if partial.StartTime != "" {
		m.StartTime = partial.StartTime
	}
	if partial.PlayerCount > 0 {
		m.PlayerCount = partial.PlayerCount
	}
	if partial.Duration > 0 {
		m.Duration = partial.Duration
	}
	m.Valid = 1
	m.HasRaw = true
	m.UpdatedAt = s.now()
	return s.saveMetaLocked(m)
}

// GameRaw returns the verbatim "all" payload for a game, if cached.
func (s *Store) GameRaw(gameNumber int) ([]byte, bool, error) {
	s.mu.RLock()
	m, ok := s.meta[gameNumber]
	s.mu.RUnlock()
	if !ok || !m.HasRaw {
		return nil, false, nil
	}
	b, err := os.ReadFile(s.rawPath(gameNumber))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

// Meta returns the metadata for a game, if known.
func (s *Store) Meta(gameNumber int) (GameMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.meta[gameNumber]
	return m, ok
}

// HasRaw reports whether the full payload for a game is cached.
func (s *Store) HasRaw(gameNumber int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta[gameNumber].HasRaw
}

// AllMeta returns every game's metadata, ascending by game number.
func (s *Store) AllMeta() []GameMeta {
	s.mu.RLock()
	out := make([]GameMeta, 0, len(s.meta))
	for _, m := range s.meta {
		out = append(out, m)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].GameNumber < out[j].GameNumber })
	return out
}

// Count returns the number of games known to the cache.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.meta)
}

// Prune removes games whose metadata was last updated before the cutoff. Returns
// the number removed. Used to enforce the retention window (e.g. 24h).
func (s *Store) Prune(before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for n, m := range s.meta {
		if m.UpdatedAt.Before(before) {
			_ = os.Remove(s.rawPath(n))
			_ = os.Remove(s.metaPath(n))
			delete(s.meta, n)
			removed++
		}
	}
	return removed, nil
}

// PurgeAll removes every cached game. Returns the number removed.
func (s *Store) PurgeAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.meta)
	for gn := range s.meta {
		_ = os.Remove(s.rawPath(gn))
		_ = os.Remove(s.metaPath(gn))
	}
	s.meta = map[int]GameMeta{}
	return n, nil
}

// Close is a no-op for the filesystem store (state is already on disk).
func (s *Store) Close() error { return nil }

func (s *Store) writeRawLocked(gameNumber int, raw []byte) error {
	return writeFileAtomic(s.rawPath(gameNumber), raw)
}

func (s *Store) saveMetaLocked(m GameMeta) error {
	s.meta[m.GameNumber] = m
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.metaPath(m.GameNumber), b)
}

func (s *Store) rawPath(n int) string { return filepath.Join(s.dir, "games", fmt.Sprintf("%d.bin", n)) }
func (s *Store) metaPath(n int) string {
	return filepath.Join(s.dir, "meta", fmt.Sprintf("%d.json", n))
}

// writeFileAtomic writes via a temp file + rename so a crash never leaves a
// half-written cache entry.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

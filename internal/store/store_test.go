package store

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestStoreAndReplayVerbatim(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	raw := []byte(`{"game":{"gamenum":9,"gamename":"TDM"},"players":{"1":{"alias":"Ava"}},"events":{}}`)
	if err := s.StoreGame(GameMeta{GameNumber: 9, GameName: "TDM", PlayerCount: 1}, raw); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.GameRaw(9)
	if err != nil || !ok {
		t.Fatalf("GameRaw: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("payload not verbatim:\n got %q\nwant %q", got, raw)
	}
}

func TestListEntryMergePreservesRaw(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// List arrives first (has endtime), then the full payload.
	if err := s.UpsertListEntry(GameMeta{
		GameNumber: 9, GameName: "TDM", Duration: 24, StartTime: "2020-01-01 10:00:00",
		EndTime: "2020-01-01 10:00:24", PlayerCount: 2, Valid: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if s.HasRaw(9) {
		t.Fatal("should not have raw before StoreGame")
	}
	if err := s.StoreGame(GameMeta{GameNumber: 9, GameType: 1, StartTime: "2020-01-01 10:00:00"},
		[]byte(`{"game":{"gamenum":9}}`)); err != nil {
		t.Fatal(err)
	}

	m, ok := s.Meta(9)
	if !ok {
		t.Fatal("missing meta")
	}
	if !m.HasRaw {
		t.Error("HasRaw should be true after StoreGame")
	}
	if m.EndTime != "2020-01-01 10:00:24" {
		t.Errorf("end time lost on merge: %q", m.EndTime)
	}
	if m.Duration != 24 {
		t.Errorf("duration lost on merge: %d", m.Duration)
	}
	if m.GameType != 1 {
		t.Errorf("game type not set: %d", m.GameType)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"game":{"gamenum":42}}`)
	if err := s.StoreGame(GameMeta{GameNumber: 42, GameName: "Solo"}, raw); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s2.GameRaw(42)
	if err != nil || !ok || !bytes.Equal(got, raw) {
		t.Fatalf("reopened store lost game: ok=%v err=%v got=%q", ok, err, got)
	}
	if m, ok := s2.Meta(42); !ok || m.GameName != "Solo" {
		t.Fatalf("reopened meta wrong: %+v", m)
	}
}

func TestPrune(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Force a fixed clock.
	base := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base.Add(-2 * time.Hour) }
	_ = s.StoreGame(GameMeta{GameNumber: 1}, []byte(`{"a":1}`))
	s.now = func() time.Time { return base }
	_ = s.StoreGame(GameMeta{GameNumber: 2}, []byte(`{"a":2}`))

	removed, err := s.Prune(base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, ok := s.Meta(1); ok {
		t.Error("game 1 should have been pruned")
	}
	if _, ok := s.Meta(2); !ok {
		t.Error("game 2 should remain")
	}
}

func TestAllMetaSorted(t *testing.T) {
	s, _ := Open(t.TempDir())
	_ = s.StoreGame(GameMeta{GameNumber: 5}, []byte(`{}`))
	_ = s.StoreGame(GameMeta{GameNumber: 2}, []byte(`{}`))
	_ = s.StoreGame(GameMeta{GameNumber: 9}, []byte(`{}`))
	all := s.AllMeta()
	if len(all) != 3 || all[0].GameNumber != 2 || all[2].GameNumber != 9 {
		t.Fatalf("AllMeta not sorted ascending: %+v", all)
	}
}

// refreshCache re-lists every game O-Zone holds once a minute and wrote a file
// per listed game every pass — thousands of writes a minute against a venue
// box's flash, for data that had not changed.
func TestUnchangedListEntryIsNotRewritten(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	entry := GameMeta{GameNumber: 7, GameName: "TDM", Duration: 601, StartTime: "s", EndTime: "e", PlayerCount: 4, Valid: 1}
	if err := s.UpsertListEntry(entry); err != nil {
		t.Fatal(err)
	}

	// Remove the file: if the next pass writes, it comes back. That is a
	// clearer signal than a modification time at this resolution.
	if err := os.Remove(s.metaPath(7)); err != nil {
		t.Fatal(err)
	}

	// A minute later O-Zone lists the same game, unchanged.
	s.now = func() time.Time { return base.Add(time.Minute) }
	if err := s.UpsertListEntry(entry); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(s.metaPath(7)); !os.IsNotExist(err) {
		t.Fatal("an unchanged entry was rewritten")
	}
	// The marker did not move either, so memory still matches the disk.
	if m, _ := s.Meta(7); !m.UpdatedAt.Equal(base) {
		t.Fatalf("UpdatedAt = %v, want it left at %v", m.UpdatedAt, base)
	}
}

func TestChangedListEntryIsWrittenImmediately(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	entry := GameMeta{GameNumber: 7, GameName: "TDM", PlayerCount: 4, Valid: 1}
	_ = s.UpsertListEntry(entry)
	if err := os.Remove(s.metaPath(7)); err != nil {
		t.Fatal(err)
	}

	// The game finishes: the list now carries an end time. Coalescing must
	// never delay something a reader would notice.
	s.now = func() time.Time { return base.Add(time.Minute) }
	finished := entry
	finished.EndTime = "2020-01-01 12:01:00"
	if err := s.UpsertListEntry(finished); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(s.metaPath(7)); err != nil {
		t.Fatalf("a changed entry was not written: %v", err)
	}
	m, _ := s.Meta(7)
	if m.EndTime != finished.EndTime {
		t.Fatalf("EndTime = %q, want %q", m.EndTime, finished.EndTime)
	}
	if !m.UpdatedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("UpdatedAt = %v, want it advanced", m.UpdatedAt)
	}
}

func TestFreshnessMarkerStillAdvancesPastTheInterval(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	entry := GameMeta{GameNumber: 7, GameName: "TDM", Valid: 1}
	_ = s.UpsertListEntry(entry)
	if err := os.Remove(s.metaPath(7)); err != nil {
		t.Fatal(err)
	}

	s.now = func() time.Time { return base.Add(metaTouchInterval + time.Minute) }
	if err := s.UpsertListEntry(entry); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(s.metaPath(7)); err != nil {
		t.Fatalf("the marker was not refreshed once the interval had passed: %v", err)
	}
}

// The guarantee the coalescing must not break. Prune deletes on UpdatedAt,
// which means "last seen in O-Zone's list", so a game the venue is still
// playing must keep its marker fresh — those are exactly the games TORN asks
// the proxy for when the print server is down.
func TestStillListedGameSurvivesRetention(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	entry := GameMeta{GameNumber: 7, GameName: "TDM", Valid: 1}

	// Two hours of one-minute refreshes, the entry never changing.
	for i := 0; i <= 120; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		s.now = func() time.Time { return at }
		if err := s.UpsertListEntry(entry); err != nil {
			t.Fatal(err)
		}
	}

	// A one-hour retention window, applied at the end of that run.
	now := base.Add(120 * time.Minute)
	removed, err := s.Prune(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("pruned %d still-listed game(s); the venue would lose the cache it is playing on", removed)
	}
	if _, ok := s.Meta(7); !ok {
		t.Fatal("game 7 should still be cached")
	}
}

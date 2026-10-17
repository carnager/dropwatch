package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"dropwatch/internal/model"
)

func watchStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertArtist(model.Artist{ID: "artist", Name: "Artist"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func watchGroup(id, date string) model.ReleaseGroup {
	return model.ReleaseGroup{ID: id, MBID: id, ArtistID: "artist", Title: id, NormTitle: id, PrimaryType: "Album", FirstReleaseDate: date, Sources: []string{"musicbrainz"}}
}

func TestDumpBaselineAndNotifications(t *testing.T) {
	s := watchStore(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tracked := map[string]bool{"artist": true}
	apply := func(groups ...model.ReleaseGroup) {
		t.Helper()
		if err := s.ApplyDump(context.Background(), tracked, map[string][]model.ReleaseGroup{"artist": groups}, now, time.UTC); err != nil {
			t.Fatal(err)
		}
	}
	base := watchGroup("baseline", "2026-09-01")
	apply(base)
	if ns, unread, err := s.Notifications(now); err != nil || len(ns) != 0 || unread != 0 {
		t.Fatalf("initial baseline made notifications: %v %d %v", ns, unread, err)
	}
	recent := watchGroup("recent", "2026-09-05")
	future := watchGroup("future", "2026-09-10")
	variant := watchGroup("live", "2026-09-05")
	variant.SecondaryTypes = []string{"Live"}
	groups := []model.ReleaseGroup{base, recent, future, variant, watchGroup("old", "1990-01-01"), watchGroup("unknown", "")}
	apply(groups...)
	apply(groups...) // A repeated dump, including after a crash, is idempotent.
	ns, unread, err := s.Notifications(now)
	if err != nil || len(ns) != 2 || unread != 2 {
		t.Fatalf("notifications = %v, unread = %d, err = %v", ns, unread, err)
	}
	if ns[0].Kind != "upcoming" || ns[1].Kind != "new" {
		t.Fatalf("unexpected notification types: %v", ns)
	}
	if err := s.ReadNotifications(now); err != nil {
		t.Fatal(err)
	}
	ns, unread, err = s.Notifications(now.AddDate(0, 0, 4))
	if err != nil || len(ns) != 3 || unread != 1 || ns[0].Kind != "released" {
		t.Fatalf("release-day reminder = %v, unread = %d, err = %v", ns, unread, err)
	}
}

func TestDumpPreservesOtherDataAndDeletedArtists(t *testing.T) {
	s := watchStore(t)
	g := watchGroup("owned", "2020")
	g.State, g.DiscogsMasterID, g.Sources = model.StateOwned, 42, []string{"musicbrainz", "discogs"}
	discogs := watchGroup("discogs-only", "2020")
	discogs.Sources = []string{"discogs"}
	if err := s.SaveReleaseGroups("artist", []model.ReleaseGroup{g, discogs}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAliases("artist", []Alias{{RGID: g.ID, NormTitle: "alternate title"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDump(context.Background(), map[string]bool{"artist": true, "deleted": true},
		map[string][]model.ReleaseGroup{"artist": {watchGroup("owned", "2021")}, "deleted": {watchGroup("absent", "2026")}}, time.Now(), time.UTC); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReleaseGroups("artist")
	if err != nil || len(got) != 2 {
		t.Fatalf("groups = %v, err = %v", got, err)
	}
	i := slices.IndexFunc(got, func(g model.ReleaseGroup) bool { return g.ID == "owned" })
	if i < 0 || got[i].State != model.StateOwned || got[i].DiscogsMasterID != 42 || !slices.Contains(got[i].Sources, "discogs") || got[i].FirstReleaseDate != "2021" {
		t.Fatalf("lost state or metadata: %v", got)
	}
	aliases, err := s.Aliases("artist")
	if err != nil || len(aliases) != 1 {
		t.Fatalf("lost aliases: %v %v", aliases, err)
	}
	if baseline, err := s.HasDumpBaseline("deleted"); err != nil || baseline {
		t.Fatalf("deleted artist gained baseline: %v %v", baseline, err)
	}
	if _, err := s.DeleteArtist("artist"); err != nil {
		t.Fatal(err)
	}
	if baseline, err := s.HasDumpBaseline("artist"); err != nil || baseline {
		t.Fatalf("untracking left baseline behind: %v %v", baseline, err)
	}
}

func TestDumpReminderDatesAndCancellation(t *testing.T) {
	s := watchStore(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(date string) {
		t.Helper()
		if err := s.ApplyDump(context.Background(), map[string]bool{"artist": true},
			map[string][]model.ReleaseGroup{"artist": {watchGroup("future", date)}}, now, loc); err != nil {
			t.Fatal(err)
		}
	}
	apply("2026-09-10")
	apply("2026-09-12")
	if ns, _, _ := s.Notifications(time.Date(2026, 9, 11, 21, 59, 0, 0, time.UTC)); len(ns) != 0 {
		t.Fatalf("postponed release notified early: %v", ns)
	}
	if ns, _, _ := s.Notifications(time.Date(2026, 9, 11, 22, 0, 0, 0, time.UTC)); len(ns) != 1 {
		t.Fatalf("reminder not due at Berlin midnight: %v", ns)
	}
	apply("2026-09")
	if ns, _, _ := s.Notifications(now.AddDate(0, 1, 0)); len(ns) != 0 {
		t.Fatalf("partial date left an invented reminder: %v", ns)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ApplyDump(ctx, map[string]bool{"artist": true}, map[string][]model.ReleaseGroup{"artist": {watchGroup("cancelled", "2026-09-06")}}, now, loc); err == nil {
		t.Fatal("cancelled snapshot succeeded")
	}
	groups, err := s.ReleaseGroups("artist")
	if err != nil || len(groups) != 1 {
		t.Fatalf("cancelled snapshot changed database: %v %v", groups, err)
	}
}

func TestDumpRollsBackPartialWrites(t *testing.T) {
	s := watchStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_test_group BEFORE INSERT ON dump_seen
		WHEN NEW.rg_id = 'fail' BEGIN SELECT RAISE(FAIL, 'simulated write failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.ApplyDump(context.Background(), map[string]bool{"artist": true}, map[string][]model.ReleaseGroup{
		"artist": {watchGroup("first", "2026-09-06"), watchGroup("fail", "2026-09-06")},
	}, time.Now(), time.UTC)
	if err == nil {
		t.Fatal("expected write failure")
	}
	groups, err := s.ReleaseGroups("artist")
	if err != nil || len(groups) != 0 {
		t.Fatalf("partial release groups committed: %v %v", groups, err)
	}
	if baseline, err := s.HasDumpBaseline("artist"); err != nil || baseline {
		t.Fatalf("failed transaction established baseline: %v %v", baseline, err)
	}
}

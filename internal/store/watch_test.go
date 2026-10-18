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
	if ns, unread, err := s.Notifications(now, nil); err != nil || len(ns) != 1 || unread != 1 || ns[0].Kind != "new" {
		t.Fatalf("initial baseline should announce the recent release: %v %d %v", ns, unread, err)
	}
	recent := watchGroup("recent", "2026-09-05")
	future := watchGroup("future", "2026-09-10")
	variant := watchGroup("live", "2026-09-05")
	variant.SecondaryTypes = []string{"Live"}
	groups := []model.ReleaseGroup{base, recent, future, variant, watchGroup("old", "1990-01-01"), watchGroup("unknown", "")}
	apply(groups...)
	apply(groups...) // A repeated dump, including after a crash, is idempotent.
	ns, unread, err := s.Notifications(now, nil)
	if err != nil || len(ns) != 3 || unread != 3 {
		t.Fatalf("notifications = %v, unread = %d, err = %v", ns, unread, err)
	}
	if ns[0].Kind != "upcoming" || ns[1].Kind != "new" || ns[2].ReleaseID != "baseline" {
		t.Fatalf("unexpected notification types: %v", ns)
	}
	if err := s.ReadNotifications(now, nil); err != nil {
		t.Fatal(err)
	}
	ns, unread, err = s.Notifications(now.AddDate(0, 0, 4), nil)
	if err != nil || len(ns) != 4 || unread != 1 || ns[0].Kind != "released" {
		t.Fatalf("release-day reminder = %v, unread = %d, err = %v", ns, unread, err)
	}
}

func TestBackfillBaselineReleases(t *testing.T) {
	s := watchStore(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	live := watchGroup("live", "2026-09-01")
	live.SecondaryTypes = []string{"Live"}
	groups := []model.ReleaseGroup{watchGroup("recent", "2026-09-01"), watchGroup("old", "2020-01-01"), watchGroup("future", "2026-09-20"), live}
	if err := s.SaveReleaseGroups("artist", groups); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO dump_baselines (artist_id, created_at) VALUES ('artist', ?)`, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for range 2 { // The second call is a no-op.
		if err := s.BackfillBaselineReleases(now, time.UTC); err != nil {
			t.Fatal(err)
		}
	}
	ns, unread, err := s.Notifications(now, nil)
	if err != nil || len(ns) != 1 || unread != 1 || ns[0].ReleaseID != "recent" || ns[0].Kind != "new" {
		t.Fatalf("backfill = %v, unread = %d, err = %v", ns, unread, err)
	}
	if _, err := s.db.Exec(`UPDATE release_groups SET state = 'owned' WHERE id = 'recent'`); err != nil {
		t.Fatal(err)
	}
	if ns, unread, _ := s.Notifications(now, nil); len(ns) != 0 || unread != 0 {
		t.Fatalf("owned release still notified: %v %d", ns, unread)
	}
	if _, err := s.db.Exec(`DELETE FROM release_notifications`); err != nil {
		t.Fatal(err)
	}
	if err := s.BackfillBaselineReleases(now, time.UTC); err != nil {
		t.Fatal(err)
	}
	if ns, _, _ := s.Notifications(now, nil); len(ns) != 0 {
		t.Fatalf("backfill ran twice: %v", ns)
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
	if ns, _, _ := s.Notifications(time.Date(2026, 9, 11, 21, 59, 0, 0, time.UTC), nil); len(ns) != 0 {
		t.Fatalf("postponed release notified early: %v", ns)
	}
	if ns, _, _ := s.Notifications(time.Date(2026, 9, 11, 22, 0, 0, 0, time.UTC), nil); len(ns) != 1 {
		t.Fatalf("reminder not due at Berlin midnight: %v", ns)
	}
	apply("2026-09")
	if ns, _, _ := s.Notifications(now.AddDate(0, 1, 0), nil); len(ns) != 0 {
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

func TestDropGuestAliasesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertArtist(model.Artist{ID: "dfb", Name: "Deutsches Filmorchester Babelsberg"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveArtistAliases("dfb", "Deutsches Filmorchester Babelsberg", []string{"Dear Reader & Deutsches Filmorchester Babelsberg", "Filmorchester Babelsberg"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM dump_settings WHERE key = 'guest_aliases_dropped'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, found, _ := s.ArtistByName("Dear Reader & Deutsches Filmorchester Babelsberg"); found {
		t.Fatal("guest credit still resolves to the orchestra")
	}
	if _, found, _ := s.ArtistByName("Filmorchester Babelsberg"); !found {
		t.Fatal("registered alias dropped")
	}
}

func TestNotificationTypeFilter(t *testing.T) {
	s := watchStore(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	ep, single := watchGroup("ep", "2026-09-02"), watchGroup("single", "2026-09-03")
	ep.PrimaryType, single.PrimaryType = "EP", "Single"
	if err := s.ApplyDump(context.Background(), map[string]bool{"artist": true},
		map[string][]model.ReleaseGroup{"artist": {watchGroup("album", "2026-09-01"), ep, single}}, now, time.UTC); err != nil {
		t.Fatal(err)
	}
	ns, unread, err := s.Notifications(now, []string{"Album", "EP"})
	if err != nil || len(ns) != 2 || unread != 2 || ns[0].PrimaryType != "EP" || ns[1].PrimaryType != "Album" {
		t.Fatalf("filtered = %+v, unread = %d, err = %v", ns, unread, err)
	}
	if err := s.ReadNotifications(now, []string{"Single"}); err != nil {
		t.Fatal(err)
	}
	if _, unread, _ := s.Notifications(now, nil); unread != 2 {
		t.Fatalf("reading singles touched other types: unread = %d", unread)
	}
}

func TestReadSingleNotification(t *testing.T) {
	s := watchStore(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.ApplyDump(context.Background(), map[string]bool{"artist": true},
		map[string][]model.ReleaseGroup{"artist": {watchGroup("a", "2026-09-01"), watchGroup("b", "2026-09-02")}}, now, time.UTC); err != nil {
		t.Fatal(err)
	}
	ns, _, _ := s.Notifications(now, nil)
	if found, err := s.ReadNotification(ns[0].ID); err != nil || !found {
		t.Fatalf("read: %v %v", found, err)
	}
	if found, _ := s.ReadNotification(99999); found {
		t.Fatal("unknown id reported found")
	}
	ns, unread, _ := s.Notifications(now, nil)
	if unread != 1 || ns[0].Unread || !ns[1].Unread {
		t.Fatalf("after reading one: %+v unread=%d", ns, unread)
	}
}

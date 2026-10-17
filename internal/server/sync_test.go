package server

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"dropwatch/internal/model"
	"dropwatch/internal/store"
	"dropwatch/internal/watch"
)

func syncStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func trackArtist(t *testing.T, st *store.Store, id, name string, titles ...string) {
	t.Helper()
	if err := st.UpsertArtist(model.Artist{ID: id, Name: name}); err != nil {
		t.Fatal(err)
	}
	var groups []model.ReleaseGroup
	for _, title := range titles {
		groups = append(groups, model.ReleaseGroup{ID: id + "/" + title, MBID: id + "/" + title, ArtistID: id, Title: title,
			NormTitle: model.NormalizeTitle(title), PrimaryType: "Album", Sources: []string{"musicbrainz"}})
	}
	if err := st.SaveReleaseGroups(id, groups); err != nil {
		t.Fatal(err)
	}
}

func owned(t *testing.T, st *store.Store, artistID string) []string {
	t.Helper()
	groups, err := st.ReleaseGroups(artistID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range groups {
		if g.State == model.StateOwned {
			out = append(out, g.Title)
		}
	}
	return out
}

func TestSyncJointCreditResolvesToLeadArtist(t *testing.T) {
	st := syncStore(t)
	trackArtist(t, st, "dear-reader", "Dear Reader", "Rivonia", "We Followed Every Sound")
	trackArtist(t, st, "nick-cave", "Nick Cave", "Carnage")
	s := New(st, nil, "test") // A live MusicBrainz call would panic on the nil client.
	s.runSyncArtists(&mpdJob{Running: true}, []syncArtist{{
		Name:   "Dear Reader & Deutsches Filmorchester Babelsberg",
		Albums: []syncAlbum{{Title: "We Followed Every Sound"}},
	}}, true)
	if got := owned(t, st, "dear-reader"); len(got) != 1 || got[0] != "We Followed Every Sound" {
		t.Fatalf("owned = %v", got)
	}
	if a, found, _ := st.ArtistByName("Dear Reader & Deutsches Filmorchester Babelsberg"); !found || a.ID != "dear-reader" {
		t.Fatalf("joint name not remembered: %v %v", a, found)
	}
	// No album in the lead's discography: the band is not Nick Cave solo.
	if id := s.leadArtist(syncArtist{Name: "Nick Cave & The Bad Seeds", Albums: []syncAlbum{{Title: "Murder Ballads"}}}); id != "" {
		t.Fatalf("band collapsed into solo artist %q", id)
	}
}

func TestSyncResolvesNewArtistsFromKeptDump(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz required")
	}
	st := syncStore(t)
	dir := t.TempDir()
	data := `{"id":"rg-geo","title":"Geogaddi","primary-type":"Album","artist-credit":[{"name":"Boards of Canada","joinphrase":"","artist":{"id":"boc","name":"Boards of Canada","sort-name":"Boards of Canada"}}]}
{"id":"rg-twoism","title":"Twoism","primary-type":"EP","artist-credit":[{"name":"Boards of Canada","joinphrase":"","artist":{"id":"boc","name":"Boards of Canada","sort-name":"Boards of Canada"}}]}
`
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	tw.WriteHeader(&tar.Header{Name: "mbdump/release-group", Mode: 0600, Size: int64(len(data))})
	tw.Write([]byte(data))
	tw.Close()
	cmd := exec.Command("xz", "-c")
	cmd.Stdin = &raw
	compressed, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release-group.tar.xz"), compressed, 0644); err != nil {
		t.Fatal(err)
	}
	watcher, err := watch.New(st, dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	s := New(st, nil, "test") // Resolution must not touch the web service.
	s.SetWatcher(watcher)
	s.runSyncArtists(&mpdJob{Running: true}, []syncArtist{{Name: "Boards of Canada", Albums: []syncAlbum{{Title: "Geogaddi"}}}}, true)
	if got := owned(t, st, "boc"); len(got) != 1 || got[0] != "Geogaddi" {
		t.Fatalf("owned = %v", got)
	}
	if groups, _ := st.ReleaseGroups("boc"); len(groups) != 2 {
		t.Fatalf("discography not imported from dump: %v", groups)
	}
}

func TestSkipExistingStillMarksNewlyOwnedAlbums(t *testing.T) {
	st := syncStore(t)
	trackArtist(t, st, "err", "Emma Ruth Rundle", "Engine of Hell", "These Killing Times")
	s := New(st, nil, "test") // Known artists must not cause web-service requests.
	job := &mpdJob{Running: true}
	s.runSyncArtists(job, []syncArtist{{Name: "Emma Ruth Rundle", Albums: []syncAlbum{{Title: "These Killing Times"}}}}, true)
	if got := owned(t, st, "err"); len(got) != 1 || got[0] != "These Killing Times" {
		t.Fatalf("owned = %v", got)
	}
	if job.Skipped != 1 || job.Owned != 1 {
		t.Fatalf("job = %+v", job)
	}
}

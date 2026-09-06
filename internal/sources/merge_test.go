package sources

import (
	"testing"

	"dropwatch/internal/model"
)

func rg(id, title, typ string, date string, src string) model.ReleaseGroup {
	return model.ReleaseGroup{
		ID: id, Title: title, NormTitle: model.NormalizeTitle(title),
		PrimaryType: typ, FirstReleaseDate: date, Sources: []string{src},
	}
}

func TestMergeFoldsDiscogsIntoMusicBrainz(t *testing.T) {
	mb := []model.ReleaseGroup{rg("mb1", "OK Computer", "Album", "1997-05-21", "musicbrainz")}
	dc := []model.ReleaseGroup{rg("discogs-9", "OK Computer (Deluxe Edition)", "Album", "1997", "discogs")}
	dc[0].DiscogsMasterID = 9

	out := Merge("artist1", mb, dc)
	if len(out) != 1 {
		t.Fatalf("want 1 merged group, got %d: %+v", len(out), out)
	}
	g := out[0]
	if g.ID != "mb1" || g.DiscogsMasterID != 9 || len(g.Sources) != 2 {
		t.Errorf("bad merge result: %+v", g)
	}
}

func TestMergeKeepsDiscogsOnlyEntries(t *testing.T) {
	mb := []model.ReleaseGroup{rg("mb1", "Album One", "Album", "2001", "musicbrainz")}
	dc := []model.ReleaseGroup{rg("discogs-5", "Rare Vinyl Only", "Album", "2003", "discogs")}

	out := Merge("artist1", mb, dc)
	if len(out) != 2 {
		t.Fatalf("want 2 groups, got %d", len(out))
	}
	// Sorted newest first.
	if out[0].ID != "discogs-5" || out[0].ArtistID != "artist1" {
		t.Errorf("discogs-only entry mishandled: %+v", out[0])
	}
}

package mpdclient

import "testing"

func TestParseLibraryGroupedTriplets(t *testing.T) {
	out := parseLibrary([]string{
		"AlbumArtist: Boards of Canada",
		"Album: Geogaddi",
		"MUSICBRAINZ_RELEASEGROUPID: mbid-1",
		"Album: Twoism",
		"AlbumArtist: Autechre",
		"Album: Tri Repetae",
		"MUSICBRAINZ_RELEASEGROUPID: mbid-2",
	})
	if len(out) != 2 || len(out[0].Albums) != 2 || out[0].Albums[0].MBID != "mbid-1" ||
		out[0].Albums[1].MBID != "" || out[1].Albums[0].MBID != "mbid-2" {
		t.Fatalf("bad parse: %+v", out)
	}
}

func TestParseLibraryRepeatedArtistAndUnknownTags(t *testing.T) {
	// Some servers repeat AlbumArtist before every album and emit custom tags.
	out := parseLibrary([]string{
		"AlbumArtist: 10 Minute Warning",
		"Album: 10 Minute Warning",
		"X-AlbumId: 4",
		"AlbumArtist: 10 Minute Warning",
		"Album: Survival of the Fittest",
		"X-AlbumId: 5",
	})
	if len(out) != 1 || len(out[0].Albums) != 2 {
		t.Fatalf("repeated artist not merged: %+v", out)
	}
}

func TestParseLibraryFlatMBIDListYieldsNothing(t *testing.T) {
	// A server that ignores the group clauses returns bare values; that must
	// parse to zero artists so Library() falls back to the plain query.
	out := parseLibrary([]string{
		"MUSICBRAINZ_RELEASEGROUPID: mbid-1",
		"MUSICBRAINZ_RELEASEGROUPID: mbid-2",
	})
	if len(out) != 0 {
		t.Fatalf("flat list should parse to no artists: %+v", out)
	}
}

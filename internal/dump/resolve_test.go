package dump

import (
	"context"
	"strings"
	"testing"

	"dropwatch/internal/model"
	"dropwatch/internal/mpdclient"
)

func TestFindCandidatesAndBest(t *testing.T) {
	credit := func(id, name string) string {
		return `{"name":"` + name + `","joinphrase":"","artist":{"id":"` + id + `","name":"` + name + `","sort-name":"` + name + `"}}`
	}
	input := strings.Join([]string{
		// Two artists share the name "Low"; only one has the library's album.
		`{"id":"rg1","title":"Things We Lost in the Fire","primary-type":"Album","artist-credit":[` + credit("low-us", "Low") + `]}`,
		`{"id":"rg2","title":"Unrelated","primary-type":"Album","artist-credit":[` + credit("low-other", "Low") + `]}`,
		`{"id":"rg3","title":"Another Unrelated","primary-type":"Album","artist-credit":[` + credit("low-other", "Low") + `]}`,
		// A joint credit matched by its full credited phrase.
		`{"id":"rg4","title":"Out of Season","primary-type":"Album","artist-credit":[{"name":"Beth Gibbons","joinphrase":" & ","artist":{"id":"beth","name":"Beth Gibbons"}},{"name":"Rustin Man","joinphrase":"","artist":{"id":"rustin","name":"Rustin Man"}}]}`,
		`not json`,
		`{"id":"rg5","title":"Compilation","artist-credit":[` + credit(model.VariousArtistsMBID, "Various Artists") + `]}`,
		// Name matches but no album in common: not confident.
		`{"id":"rg6","title":"Nothing Owned","primary-type":"Album","artist-credit":[` + credit("stranger", "Stranger") + `]}`,
	}, "\n")
	names := map[string]bool{"low": true, "beth gibbons and rustin man": true, "stranger": true, "various artists": true}
	cands, err := findCandidates(context.Background(), strings.NewReader(input), names, nil)
	if err != nil {
		t.Fatal(err)
	}
	albums := map[string][]mpdclient.Album{
		"low":                         {{Title: "Things We Lost In The Fire"}},
		"beth gibbons and rustin man": {{Title: "Out of Season"}},
		"stranger":                    {{Title: "Something Else"}},
	}
	best := Best(cands, albums, nil)
	if len(best) != 2 || best["low"].Artist.ID != "low-us" || best["beth gibbons and rustin man"].Artist.ID != "beth" {
		t.Fatalf("best = %+v", best)
	}
	if g := best["low"].Groups; len(g) != 1 || g[0].ID != "rg1" || g[0].NormTitle != model.NormalizeTitle("Things We Lost in the Fire") {
		t.Fatalf("groups = %+v", g)
	}
}

func TestMatchGroupsByReleaseAlias(t *testing.T) {
	c := &Candidate{Groups: []model.ReleaseGroup{{ID: "rg", MBID: "rg", NormTitle: model.NormalizeTitle("Aaliyah")}}}
	aliases := map[string][]Alias{"rg": {{Norm: "x", Light: "x", ReleaseMBID: "release-1"}}}
	if got := MatchGroups(c, mpdclient.Album{Title: "Anything", MBID: "release-1"}, aliases); len(got) != 1 {
		t.Fatalf("release MBID alias not matched: %v", got)
	}
	if got := MatchGroups(c, mpdclient.Album{Title: "Anything", MBID: "release-1"}, nil); len(got) != 0 {
		t.Fatalf("matched without aliases: %v", got)
	}
}

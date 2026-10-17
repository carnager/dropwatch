package model

import "testing"

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"OK Computer":                                "ok computer",
		"OK Computer (Deluxe Edition)":               "ok computer",
		"OK Computer [2017 Remaster]":                "ok computer",
		"In Rainbows (Special Edition) [Bonus Disc]": "in rainbows",
		"Foo EP":                                 "foo",
		"Mellon Collie & The Infinite Sadness":   "mellon collie and the infinite sadness",
		"Mellon Collie and the Infinite Sadness": "mellon collie and the infinite sadness",
		"…And Justice for All":                   "and justice for all",
		"Aaliyah: Edition 2004":                  "aaliyah",
		"One in a Million: Edition 2004":         "one in a million",
		"Use Your Illusion I - Remastered":       "use your illusion i",
		"Operation: Mindcrime":                   "operation mindcrime",
		"Version 2.0":                            "version 2 0",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeTitleKeepsDistinctTitles(t *testing.T) {
	a := NormalizeTitle("Amnesiac")
	b := NormalizeTitle("Kid A")
	if a == b {
		t.Errorf("distinct titles collapsed: %q vs %q", a, b)
	}
}

func TestLeadArtistAndGuestCredits(t *testing.T) {
	for name, want := range map[string]string{
		"Dear Reader & Deutsches Filmorchester Babelsberg": "Dear Reader",
		"Chelsea Wolfe, King Dude":                         "Chelsea Wolfe",
		"Wim Kratsborn feat. The Gathering":                "Wim Kratsborn",
		"Sugababes vs. Girls Aloud":                        "Sugababes",
		"Boards of Canada":                                 "",
	} {
		if got := LeadArtist(name); got != want {
			t.Errorf("LeadArtist(%q) = %q, want %q", name, got, want)
		}
	}
	own := NormalizeArtistName("Deutsches Filmorchester Babelsberg")
	for alias, guest := range map[string]bool{
		"dear reader and deutsches filmorchester babelsberg":        true,
		"dota und das filmorchester babelsberg":                     true,
		"deutsches filmorchester babelsberg and steffen kaltschmid": false,
		"filmorchester babelsberg":                                  false,
		own:                                                         false,
	} {
		if got := IsGuestCredit(alias, own); got != guest {
			t.Errorf("IsGuestCredit(%q) = %v", alias, got)
		}
	}
}

func TestNamedAs(t *testing.T) {
	marillion := Artist{Name: "Marillion", SortName: "Marillion", Aliases: []string{"Low Fat Yoghurts"}}
	uematsu := Artist{Name: "植松伸夫", SortName: "Uematsu, Nobuo"}
	for _, c := range []struct {
		a    Artist
		name string
		want bool
	}{
		{marillion, "Low Fat Getting High", false},
		{marillion, "Low Fat Yoghurts", true},
		{marillion, "marillion", true},
		{uematsu, "Nobuo Uematsu", true},
		{Artist{Name: "MO"}, "M.O", false},
		{Artist{Name: "The Ocean"}, "Ocean", true},
	} {
		if got := c.a.NamedAs(c.name); got != c.want {
			t.Errorf("%s NamedAs(%q) = %v", c.a.Name, c.name, got)
		}
	}
}

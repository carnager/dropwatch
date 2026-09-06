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

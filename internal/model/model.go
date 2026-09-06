// Package model defines the core domain types shared by sources, store and server.
package model

import (
	"regexp"
	"strings"
)

// Artist is a musical artist, keyed by MusicBrainz ID.
type Artist struct {
	ID             string `json:"id"` // MusicBrainz artist MBID
	Name           string `json:"name"`
	SortName       string `json:"sortName,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	Country        string `json:"country,omitempty"`
	DiscogsID      int64  `json:"discogsId,omitempty"`
}

// ArtistSummary is an artist with aggregate counts over their canonical
// albums/EPs (variants excluded).
type ArtistSummary struct {
	Artist
	Total   int `json:"total"`
	Owned   int `json:"owned"`
	Missing int `json:"missing"`
}

// LibraryEntry is an owned release group with its artist, for the library
// view.
type LibraryEntry struct {
	ArtistName string `json:"artistName"`
	ReleaseGroup
}

// State is the user's ownership state for a release group.
type State string

const (
	StateNone    State = ""        // not marked: counts as missing
	StateOwned   State = "owned"   // user has this release
	StateIgnored State = "ignored" // user doesn't care about this release
)

// ReleaseGroup is one logical album/EP, with all versions from all sources
// collapsed into a single entry.
type ReleaseGroup struct {
	ID               string   `json:"id"` // MBID, or "discogs-<masterID>" if Discogs-only
	ArtistID         string   `json:"artistId"`
	Title            string   `json:"title"`
	NormTitle        string   `json:"-"`
	PrimaryType      string   `json:"primaryType"` // Album, EP, Single, ...
	SecondaryTypes   []string `json:"secondaryTypes,omitempty"`
	FirstReleaseDate string   `json:"firstReleaseDate,omitempty"` // YYYY[-MM[-DD]]
	MBID             string   `json:"mbid,omitempty"`
	DiscogsMasterID  int64    `json:"discogsMasterId,omitempty"`
	Sources          []string `json:"sources"`
	State            State    `json:"state"`
	Missing          bool     `json:"missing"`
}

var (
	editionRe = regexp.MustCompile(`(?i)\s*[(\[][^)\]]*(deluxe|remaster|expanded|anniversary|bonus|special|edition|reissue|version|mono|stereo)[^)\]]*[)\]]\s*$`)
	// Reissues are also titled with bare suffixes: "Aaliyah: Edition 2004",
	// "Use Your Illusion I - Remastered". Keyword-gated so real subtitles
	// ("Operation: Mindcrime") survive.
	suffixRe = regexp.MustCompile(`(?i)\s*[:\x{2010}-\x{2015}-][^:]*\b(deluxe|remaster(ed)?|expanded|anniversary|bonus|edition|reissue|version)\b[^:]*$`)
	nonAlnum = regexp.MustCompile(`[^a-z0-9 ]+`)
	spaces   = regexp.MustCompile(`\s+`)
)

// NormalizeTitle reduces an album title to a comparison key so the same album
// from different sources (or different editions) groups together.
func NormalizeTitle(title string) string {
	t := strings.ToLower(title)
	// Strip trailing edition qualifiers, possibly stacked and in either
	// style: "(Deluxe Edition)" / "[2017 Remaster]" / ": Edition 2004".
	for {
		stripped := editionRe.ReplaceAllString(t, "")
		stripped = suffixRe.ReplaceAllString(stripped, "")
		if stripped == t {
			break
		}
		t = stripped
	}
	t = strings.NewReplacer("&", " and ", "+", " and ").Replace(t)
	t = nonAlnum.ReplaceAllString(t, " ")
	t = spaces.ReplaceAllString(strings.TrimSpace(t), " ")
	// A trailing " ep" is formatting, not identity: "Foo EP" == "Foo".
	t = strings.TrimSuffix(t, " ep")
	return strings.TrimSpace(t)
}

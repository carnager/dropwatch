package sources

import (
	"slices"
	"strings"

	"dropwatch/internal/model"
)

// isRepackaging reports whether a title like "Album One / Album Two" is just
// existing releases bundled together (Discogs lists such box sets as their
// own masters).
func isRepackaging(title string, byNorm map[string]*model.ReleaseGroup) bool {
	parts := strings.Split(title, "/")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if _, ok := byNorm[model.NormalizeTitle(p)]; !ok {
			return false
		}
	}
	return true
}

// Merge combines MusicBrainz release groups (authoritative) with Discogs
// masters. Discogs entries whose normalized title matches an existing MB group
// are folded into it; the rest are appended as Discogs-only groups. This keeps
// one entry per logical album regardless of how many sources or editions
// carry it.
func Merge(artistID string, mb, discogs []model.ReleaseGroup) []model.ReleaseGroup {
	byNorm := make(map[string]*model.ReleaseGroup, len(mb))
	out := make([]model.ReleaseGroup, 0, len(mb)+len(discogs))
	for _, g := range mb {
		out = append(out, g)
	}
	for i := range out {
		// First MB group wins a normalized title; later ones (e.g. an album
		// and a live album sharing a name) stay separate entries but don't
		// absorb Discogs matches twice.
		if _, ok := byNorm[out[i].NormTitle]; !ok {
			byNorm[out[i].NormTitle] = &out[i]
		}
	}
	for _, dg := range discogs {
		if isRepackaging(dg.Title, byNorm) {
			continue
		}
		if existing, ok := byNorm[dg.NormTitle]; ok {
			if !slices.Contains(existing.Sources, "discogs") {
				existing.Sources = append(existing.Sources, "discogs")
			}
			if existing.DiscogsMasterID == 0 {
				existing.DiscogsMasterID = dg.DiscogsMasterID
			}
			continue
		}
		dg.ArtistID = artistID
		if strings.HasSuffix(strings.ToLower(dg.Title), " ep") {
			dg.PrimaryType = "EP"
		}
		out = append(out, dg)
		g := &out[len(out)-1]
		byNorm[g.NormTitle] = g
	}
	slices.SortFunc(out, func(a, b model.ReleaseGroup) int {
		// Newest first; undated entries sink to the bottom.
		if a.FirstReleaseDate != b.FirstReleaseDate {
			return strings.Compare(b.FirstReleaseDate, a.FirstReleaseDate)
		}
		return strings.Compare(a.Title, b.Title)
	})
	return out
}

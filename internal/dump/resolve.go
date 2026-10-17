package dump

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"dropwatch/internal/model"
	"dropwatch/internal/mpdclient"
)

// Candidate is one MusicBrainz artist whose name (or credited form) matched a
// library artist. When several share a name, Best picks the one that actually
// has the library's albums.
type Candidate struct {
	Artist model.Artist
	LibKey string // model.NormalizeArtistName of the library name it matched
	Groups []model.ReleaseGroup
}

// Alias is one release title inside a release group, from the release dump.
type Alias struct {
	Norm, Light, ReleaseMBID string
}

// Open streams a dump given as the downloaded .tar.xz or as extracted NDJSON
// (much faster: no xz decompression). Call the returned close func after
// reading; it reports tar failures.
func Open(ctx context.Context, path string) (io.Reader, func() error, error) {
	if strings.HasSuffix(path, ".tar.xz") || strings.HasSuffix(path, ".txz") {
		cmd := exec.CommandContext(ctx, "tar", "-xJOf", path, "--wildcards", "mbdump/*")
		cmd.Stderr = os.Stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, fmt.Errorf("starting tar (tar and xz are required): %w", err)
		}
		return stdout, cmd.Wait, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// FindCandidates scans a release-group dump for artists credited under any of
// the given normalized library names. Malformed records are skipped: a
// user-supplied extract may not be pristine, and a missed record only costs a
// match, never a wrong one.
func FindCandidates(ctx context.Context, path string, names map[string]bool, progress func(records int)) ([]*Candidate, error) {
	input, closeDump, err := Open(ctx, path)
	if err != nil {
		return nil, err
	}
	cands, err := findCandidates(ctx, input, names, progress)
	closeErr := closeDump()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, fmt.Errorf("reading dump: %w", closeErr)
	}
	return cands, nil
}

func findCandidates(ctx context.Context, input io.Reader, names map[string]bool, progress func(int)) ([]*Candidate, error) {
	type rgLine struct {
		ID               string   `json:"id"`
		Title            string   `json:"title"`
		PrimaryType      string   `json:"primary-type"`
		SecondaryTypes   []string `json:"secondary-types"`
		FirstReleaseDate string   `json:"first-release-date"`
		ArtistCredit     []struct {
			Name       string `json:"name"`
			JoinPhrase string `json:"joinphrase"`
			Artist     struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				SortName string `json:"sort-name"`
			} `json:"artist"`
		} `json:"artist-credit"`
	}
	byID := map[string]*Candidate{}
	var order []*Candidate
	record := func(artistID, artistName, sortName, libKey string, rg rgLine) {
		c, ok := byID[artistID+"\x00"+libKey]
		if !ok {
			c = &Candidate{Artist: model.Artist{ID: artistID, Name: artistName, SortName: sortName}, LibKey: libKey}
			byID[artistID+"\x00"+libKey] = c
			order = append(order, c)
		}
		pt := rg.PrimaryType
		if pt == "" {
			pt = "Other"
		}
		c.Groups = append(c.Groups, model.ReleaseGroup{
			ID: rg.ID, ArtistID: artistID, Title: rg.Title,
			NormTitle: model.NormalizeTitle(rg.Title), PrimaryType: pt,
			SecondaryTypes: rg.SecondaryTypes, FirstReleaseDate: rg.FirstReleaseDate,
			MBID: rg.ID, Sources: []string{"musicbrainz"},
		})
	}
	reader := bufio.NewReaderSize(input, 1<<20)
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		var rg rgLine
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &rg) == nil && len(rg.ArtistCredit) > 0 {
			count++
			if progress != nil && count%250000 == 0 {
				progress(count)
			}
			for _, credit := range rg.ArtistCredit {
				if credit.Artist.ID == model.VariousArtistsMBID || credit.Artist.ID == model.UnknownArtistMBID {
					continue
				}
				if key := model.NormalizeArtistName(credit.Artist.Name); names[key] {
					record(credit.Artist.ID, credit.Artist.Name, credit.Artist.SortName, key, rg)
				} else if key := model.NormalizeArtistName(credit.Name); credit.Name != "" && names[key] {
					// Credited form differs from the artist's name ("Star Anna
					// and the Laughing Dogs" crediting "Star Anna").
					record(credit.Artist.ID, credit.Artist.Name, credit.Artist.SortName, key, rg)
				}
			}
			// Joint credits ("Simon & Garfunkel" as two artists): match the full
			// credited phrase too, attributed to the first credited artist.
			if len(rg.ArtistCredit) > 1 {
				var full strings.Builder
				for _, credit := range rg.ArtistCredit {
					full.WriteString(credit.Name)
					full.WriteString(credit.JoinPhrase)
				}
				first := rg.ArtistCredit[0].Artist
				if key := model.NormalizeArtistName(full.String()); names[key] && first.ID != model.VariousArtistsMBID && first.ID != model.UnknownArtistMBID {
					record(first.ID, full.String(), full.String(), key, rg)
				}
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("release-group dump is empty")
	}
	return order, nil
}

// MatchGroups finds the candidate's release groups matching one library
// album: by MBID (release group, or release via aliases), by normalized group
// title, or by any release-title alias.
func MatchGroups(c *Candidate, alb mpdclient.Album, aliases map[string][]Alias) []*model.ReleaseGroup {
	norm := model.NormalizeTitle(alb.Title)
	light := model.NormalizeTitleLight(alb.Title)
	var out []*model.ReleaseGroup
	for i := range c.Groups {
		g := &c.Groups[i]
		ok := (alb.MBID != "" && g.MBID == alb.MBID) || g.NormTitle == norm
		if !ok {
			for _, a := range aliases[g.ID] {
				if a.Norm == norm || a.Light == light || (alb.MBID != "" && a.ReleaseMBID == alb.MBID) {
					ok = true
					break
				}
			}
		}
		if ok {
			out = append(out, g)
		}
	}
	return out
}

// Best picks, per library name, the candidate with the most library albums in
// its discography, then the largest discography. Only confident matches are
// returned: the name matched AND at least one library album was found.
// albums is keyed like Candidate.LibKey.
func Best(cands []*Candidate, albums map[string][]mpdclient.Album, aliases map[string][]Alias) map[string]*Candidate {
	type scored struct {
		c     *Candidate
		score int
	}
	best := map[string]scored{}
	for _, c := range cands {
		lib, ok := albums[c.LibKey]
		if !ok {
			continue
		}
		n := 0
		for _, alb := range lib {
			if len(MatchGroups(c, alb, aliases)) > 0 {
				n++
			}
		}
		cur, ok := best[c.LibKey]
		if !ok || n > cur.score || (n == cur.score && len(c.Groups) > len(cur.c.Groups)) {
			best[c.LibKey] = scored{c, n}
		}
	}
	out := make(map[string]*Candidate, len(best))
	for key, s := range best {
		if s.score > 0 {
			out[key] = s.c
		}
	}
	return out
}

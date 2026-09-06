package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"dropwatch/internal/model"
	"dropwatch/internal/mpdclient"
	"dropwatch/internal/store"
)

// runImport bulk-populates the database from a MusicBrainz release-group JSON
// dump instead of the live API — the sane path for an initial import of a
// large library. It reads the library from MPD, streams the dump once,
// matches release groups to library artists via the embedded artist credits,
// and marks owned albums in the same pass. Discogs data is added later by the
// normal live refresh.
func runImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dbPath := fs.String("db", "dropwatch.db", "path to SQLite database")
	dumpPath := fs.String("dump", "", "path to MusicBrainz release-group JSON dump (release-group.tar.xz or extracted NDJSON)")
	releaseDump := fs.String("release-dump", "", "optional path to the MusicBrainz release JSON dump (release.tar.xz or extracted NDJSON); adds release-title aliases so reissue-titled rips match")
	mpdAddr := fs.String("mpd", os.Getenv("MPD_HOST"), "MPD server address (host[:port]); also read from MPD_HOST")
	fs.Parse(args)
	if *dumpPath == "" || *mpdAddr == "" {
		log.Fatal("usage: dropwatch import -dump release-group.tar.xz [-release-dump release.tar.xz] -mpd host[:port] [-db dropwatch.db]\n" +
			"download the dumps from https://data.metabrainz.org/pub/musicbrainz/data/json-dumps/ (newest date directory)")
	}

	host, password := *mpdAddr, os.Getenv("MPD_PASSWORD")
	if pw, h, ok := strings.Cut(host, "@"); ok {
		password, host = pw, h
	}
	client, err := mpdclient.Dial(host, password)
	if err != nil {
		log.Fatalf("mpd: %v", err)
	}
	library, err := client.Library()
	client.Close()
	if err != nil {
		log.Fatalf("mpd: %v", err)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	// Index the library by normalized artist name. Already-tracked artists
	// are skipped: the import must not clobber live-synced data, and this
	// makes re-runs cheap.
	type libArtist struct {
		name   string
		albums []mpdclient.Album
	}
	libByNorm := map[string]*libArtist{}
	skippedTracked, skippedVA := 0, 0
	for _, a := range library {
		norm := normalizeName(a.Name)
		if norm == "" || norm == "various artists" || norm == "va" {
			skippedVA++
			continue
		}
		if _, tracked, err := st.ArtistByName(a.Name); err == nil && tracked {
			skippedTracked++
			continue
		}
		libByNorm[norm] = &libArtist{name: a.Name, albums: a.Albums}
	}
	log.Printf("library: %d artists to import (%d already tracked, %d various-artists entries skipped)",
		len(libByNorm), skippedTracked, skippedVA)
	if len(libByNorm) == 0 {
		log.Print("nothing to do")
		return
	}

	// candidate is one MusicBrainz artist that matched a library name. When
	// several share a name, the one that actually has the library's albums
	// wins.
	type candidate struct {
		artist model.Artist
		libKey string
		groups []model.ReleaseGroup
	}
	candidates := map[string]*candidate{}

	const variousArtistsMBID = "89ad4ac3-39f7-470e-963a-56509c546377"

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

	sc, closeDump, err := openDump(*dumpPath)
	if err != nil {
		log.Fatalf("open dump: %v", err)
	}

	matchedNames := map[string]bool{}
	record := func(artistID, artistName, sortName, libKey string, rg rgLine) {
		matchedNames[libKey] = true
		c, ok := candidates[artistID]
		if !ok {
			c = &candidate{
				artist: model.Artist{ID: artistID, Name: artistName, SortName: sortName},
				libKey: libKey,
			}
			candidates[artistID] = c
		}
		pt := rg.PrimaryType
		if pt == "" {
			pt = "Other"
		}
		c.groups = append(c.groups, model.ReleaseGroup{
			ID: rg.ID, ArtistID: artistID, Title: rg.Title,
			NormTitle: model.NormalizeTitle(rg.Title), PrimaryType: pt,
			SecondaryTypes: rg.SecondaryTypes, FirstReleaseDate: rg.FirstReleaseDate,
			MBID: rg.ID, Sources: []string{"musicbrainz"},
		})
	}

	lines := 0
	for sc.Scan() {
		lines++
		if lines%250000 == 0 {
			log.Printf("scanned %dk release groups — %d/%d library artists seen (%d same-name candidates to disambiguate)",
				lines/1000, len(matchedNames), len(libByNorm), len(candidates))
		}
		var rg rgLine
		if err := json.Unmarshal(sc.Bytes(), &rg); err != nil || len(rg.ArtistCredit) == 0 {
			continue
		}
		for _, credit := range rg.ArtistCredit {
			if credit.Artist.ID == variousArtistsMBID {
				continue
			}
			if key := normalizeName(credit.Artist.Name); libByNorm[key] != nil {
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
			if key := normalizeName(full.String()); libByNorm[key] != nil && first.ID != variousArtistsMBID {
				record(first.ID, full.String(), full.String(), key, rg)
			}
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatalf("reading dump: %v", err)
	}
	if err := closeDump(); err != nil {
		log.Fatalf("dump: %v", err)
	}
	log.Printf("dump scanned: %d release groups — %d/%d library artists seen (%d candidates)",
		lines, len(matchedNames), len(libByNorm), len(candidates))

	// Optional second pass over the release dump: collect the titles of every
	// release inside a candidate group. Release titles carry reissue names
	// ("Aaliyah: Edition 2004") that group titles don't, so they make both
	// confidence scoring and owned-marking far more reliable.
	type aliasRec struct{ norm, light, relMBID string }
	rgAliases := map[string][]aliasRec{}
	if *releaseDump != "" {
		wanted := make(map[string]bool)
		for _, c := range candidates {
			for _, g := range c.groups {
				wanted[g.ID] = true
			}
		}
		rsc, closeRel, err := openDump(*releaseDump)
		if err != nil {
			log.Fatalf("open release dump: %v", err)
		}
		type relLine struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			ReleaseGroup struct {
				ID string `json:"id"`
			} `json:"release-group"`
		}
		rlines, withRG, matched := 0, 0, 0
		for rsc.Scan() {
			rlines++
			if rlines%500000 == 0 {
				log.Printf("scanned %dk releases — %d alias titles collected", rlines/1000, matched)
			}
			var r relLine
			if err := json.Unmarshal(rsc.Bytes(), &r); err != nil || r.ReleaseGroup.ID == "" {
				continue
			}
			withRG++
			if !wanted[r.ReleaseGroup.ID] {
				continue
			}
			matched++
			rgAliases[r.ReleaseGroup.ID] = append(rgAliases[r.ReleaseGroup.ID], aliasRec{
				norm:    model.NormalizeTitle(r.Title),
				light:   model.NormalizeTitleLight(r.Title),
				relMBID: r.ID,
			})
		}
		if err := rsc.Err(); err != nil {
			log.Fatalf("reading release dump: %v", err)
		}
		if err := closeRel(); err != nil {
			log.Fatalf("release dump: %v", err)
		}
		if rlines > 0 && withRG == 0 {
			log.Fatal("release dump contains no release-group references — is this the right file?")
		}
		log.Printf("release dump scanned: %d releases, %d alias titles across %d groups", rlines, matched, len(rgAliases))
	}

	// matchGroups finds the candidate's release groups matching one library
	// album: by release-group MBID tag, by normalized group title, or by any
	// release-title alias.
	matchGroups := func(c *candidate, alb mpdclient.Album) []*model.ReleaseGroup {
		norm := model.NormalizeTitle(alb.Title)
		light := model.NormalizeTitleLight(alb.Title)
		var out []*model.ReleaseGroup
		for i := range c.groups {
			g := &c.groups[i]
			ok := (alb.MBID != "" && g.MBID == alb.MBID) || g.NormTitle == norm
			if !ok {
				for _, a := range rgAliases[g.ID] {
					if a.norm == norm || a.light == light {
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

	// Pick the best candidate per library artist: most owned-album matches,
	// then largest discography. Only confident matches are imported — the
	// name matched AND at least one library album is in the candidate's
	// discography. Anything weaker is left for the live sync, which has
	// proper search scoring.
	type scored struct {
		c     *candidate
		score int
	}
	best := map[string]scored{}
	for _, c := range candidates {
		lib := libByNorm[c.libKey]
		if lib == nil {
			continue
		}
		n := 0
		for _, alb := range lib.albums {
			if len(matchGroups(c, alb)) > 0 {
				n++
			}
		}
		cur, ok := best[c.libKey]
		if !ok || n > cur.score || (n == cur.score && len(c.groups) > len(cur.c.groups)) {
			best[c.libKey] = scored{c, n}
		}
	}

	imported, ownedTotal := 0, 0
	var unmatched []string
	for key, lib := range libByNorm {
		s := best[key]
		if s.c == nil || s.score == 0 {
			unmatched = append(unmatched, lib.name)
			continue
		}
		c := s.c
		if err := st.UpsertArtist(c.artist); err != nil {
			log.Fatalf("save artist %s: %v", c.artist.Name, err)
		}
		if err := st.SaveReleaseGroups(c.artist.ID, c.groups); err != nil {
			log.Fatalf("save release groups for %s: %v", c.artist.Name, err)
		}
		if len(rgAliases) > 0 {
			var rows []store.Alias
			for _, g := range c.groups {
				for _, a := range rgAliases[g.ID] {
					rows = append(rows, store.Alias{RGID: g.ID, NormTitle: a.norm, ReleaseMBID: a.relMBID})
					if a.light != a.norm {
						rows = append(rows, store.Alias{RGID: g.ID, NormTitle: a.light, ReleaseMBID: a.relMBID})
					}
				}
			}
			if err := st.SaveAliases(c.artist.ID, rows); err != nil {
				log.Fatalf("save aliases for %s: %v", c.artist.Name, err)
			}
		}
		owned := 0
		for _, alb := range lib.albums {
			matched := matchGroups(c, alb)
			if len(matched) == 0 {
				continue
			}
			for _, g := range model.PreferCanonical(matched) {
				st.SetState(g.ID, model.StateOwned)
			}
			owned++
		}
		ownedTotal += owned
		imported++
		log.Printf("imported %s: %d release groups, %d/%d albums owned", c.artist.Name, len(c.groups), owned, len(lib.albums))
	}

	fmt.Printf("\nimported %d/%d artists (confident matches only), marked %d albums owned\n", imported, len(libByNorm), ownedTotal)
	if len(unmatched) > 0 {
		fmt.Printf("%d artists had no confident dump match and were left out:\n", len(unmatched))
		for _, n := range unmatched {
			fmt.Printf("  %s\n", n)
		}
		fmt.Println("→ run \"sync with mpd\" in the web UI with \"only new artists\" checked to resolve these via the live API.")
	}
	fmt.Println("note: Discogs data is not in the dump — it is merged in per artist on the next refresh.")
}

// openDump streams a MusicBrainz JSON dump line-by-line, accepting either the
// .tar.xz as downloaded or the extracted NDJSON file (much faster — no xz
// decompression). Gigabytes are never buffered; call the returned close func
// after scanning.
func openDump(path string) (*bufio.Scanner, func() error, error) {
	var reader io.Reader
	var closeFn func() error
	if strings.HasSuffix(path, ".tar.xz") || strings.HasSuffix(path, ".txz") {
		cmd := exec.Command("tar", "-xJOf", path, "--wildcards", "mbdump/*")
		cmd.Stderr = os.Stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, fmt.Errorf("starting tar (is xz installed?): %w", err)
		}
		reader, closeFn = stdout, cmd.Wait
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		reader, closeFn = f, f.Close
	}
	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	return sc, closeFn, nil
}

var nameNonAlnum = regexp.MustCompile(`[^a-z0-9 ]+`)

// normalizeName reduces an artist name to a comparison key. Both the library
// tags and the dump's credit names go through this, so it only has to be
// consistent, not perfect.
func normalizeName(name string) string {
	n := strings.ToLower(name)
	n = strings.NewReplacer("&", " and ", "+", " and ").Replace(n)
	n = nameNonAlnum.ReplaceAllString(n, " ")
	n = strings.Join(strings.Fields(n), " ")
	n = strings.TrimPrefix(n, "the ")
	return n
}

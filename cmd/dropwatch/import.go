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
	dumpPath := fs.String("dump", "", "path to MusicBrainz release-group JSON dump (release-group.tar.xz)")
	mpdAddr := fs.String("mpd", os.Getenv("MPD_HOST"), "MPD server address (host[:port]); also read from MPD_HOST")
	fs.Parse(args)
	if *dumpPath == "" || *mpdAddr == "" {
		log.Fatal("usage: dropwatch import -dump release-group.tar.xz -mpd host[:port] [-db dropwatch.db]\n" +
			"download the dump from https://data.metabrainz.org/pub/musicbrainz/data/json-dumps/ (newest date directory)")
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
		name       string
		albums     []mpdclient.Album
		albumNorms map[string]bool
		albumMBIDs map[string]bool
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
		la := &libArtist{name: a.Name, albums: a.Albums,
			albumNorms: map[string]bool{}, albumMBIDs: map[string]bool{}}
		for _, alb := range a.Albums {
			la.albumNorms[model.NormalizeTitle(alb.Title)] = true
			if alb.MBID != "" {
				la.albumMBIDs[alb.MBID] = true
			}
		}
		libByNorm[norm] = la
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

	// The dump can be the .tar.xz as downloaded or the already-extracted
	// NDJSON file (much faster — no xz decompression). Either way the data
	// streams; gigabytes are never buffered.
	var reader io.Reader
	var cmd *exec.Cmd
	if strings.HasSuffix(*dumpPath, ".tar.xz") || strings.HasSuffix(*dumpPath, ".txz") {
		cmd = exec.Command("tar", "-xJOf", *dumpPath, "--wildcards", "mbdump/*")
		cmd.Stderr = os.Stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			log.Fatalf("starting tar (is xz installed?): %v", err)
		}
		reader = stdout
	} else {
		f, err := os.Open(*dumpPath)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		reader = f
	}
	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 1<<20), 32<<20)

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
	if cmd != nil {
		if err := cmd.Wait(); err != nil {
			log.Fatalf("tar: %v", err)
		}
	}
	log.Printf("dump scanned: %d release groups — %d/%d library artists seen (%d candidates)",
		lines, len(matchedNames), len(libByNorm), len(candidates))

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
		for _, g := range c.groups {
			if lib.albumMBIDs[g.MBID] || lib.albumNorms[g.NormTitle] {
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
		owned := 0
		for _, g := range c.groups {
			if lib.albumMBIDs[g.MBID] || lib.albumNorms[g.NormTitle] {
				if _, err := st.SetState(g.ID, model.StateOwned); err == nil {
					owned++
				}
			}
		}
		ownedTotal += owned
		imported++
		log.Printf("imported %s: %d release groups, %d owned", c.artist.Name, len(c.groups), owned)
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

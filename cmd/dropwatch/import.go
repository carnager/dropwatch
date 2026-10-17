package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	"dropwatch/internal/dump"
	"dropwatch/internal/model"
	"dropwatch/internal/mpdclient"
	"dropwatch/internal/store"
	"dropwatch/internal/subsonic"
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
	subsonicURL := fs.String("subsonic", os.Getenv("SUBSONIC_URL"), "Subsonic-compatible server URL; also read from SUBSONIC_URL (credentials via SUBSONIC_USER/SUBSONIC_PASSWORD)")
	melodyAddr := fs.String("melody", os.Getenv("MELODY_SERVER"), "melody engine address (host[:port]); also read from MELODY_SERVER (password via MELODY_PASSWORD or MELODY_PASSWORD_FILE)")
	fs.Parse(args)
	// An explicitly passed player flag beats the others' env-var defaults:
	// "-subsonic URL" must not be shadowed by an inherited MPD_HOST.
	players := map[string]*string{"mpd": mpdAddr, "subsonic": subsonicURL, "melody": melodyAddr}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		if players[f.Name] != nil {
			explicit[f.Name] = true
		}
	})
	for name, value := range players {
		if len(explicit) > 0 && !explicit[name] {
			*value = ""
		}
	}
	if *dumpPath == "" || (*mpdAddr == "" && *subsonicURL == "" && *melodyAddr == "") {
		log.Fatal("usage: dropwatch import -dump release-group.tar.xz [-release-dump release.tar.xz] (-mpd host[:port] | -subsonic URL | -melody host[:port]) [-db dropwatch.db]\n" +
			"download the dumps from https://data.metabrainz.org/pub/musicbrainz/data/json-dumps/ (newest date directory)")
	}

	library, err := readLibrary(*mpdAddr, *subsonicURL, *melodyAddr)
	if err != nil {
		log.Fatal(err)
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

	names := make(map[string]bool, len(libByNorm))
	libAlbums := make(map[string][]mpdclient.Album, len(libByNorm))
	for key, lib := range libByNorm {
		names[key] = true
		libAlbums[key] = lib.albums
	}
	candidates, err := dump.FindCandidates(context.Background(), *dumpPath, names, func(records int) {
		log.Printf("scanned %dk release groups", records/1000)
	})
	if err != nil {
		log.Fatalf("reading dump: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.LibKey] = true
	}
	log.Printf("dump scanned — %d/%d library artists seen (%d candidates)", len(seen), len(libByNorm), len(candidates))

	// Optional second pass over the release dump: collect the titles of every
	// release inside a candidate group. Release titles carry reissue names
	// ("Aaliyah: Edition 2004") that group titles don't, so they make both
	// confidence scoring and owned-marking far more reliable.
	rgAliases := map[string][]dump.Alias{}
	if *releaseDump != "" {
		wanted := make(map[string]bool)
		for _, c := range candidates {
			for _, g := range c.Groups {
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
			rgAliases[r.ReleaseGroup.ID] = append(rgAliases[r.ReleaseGroup.ID], dump.Alias{
				Norm:        model.NormalizeTitle(r.Title),
				Light:       model.NormalizeTitleLight(r.Title),
				ReleaseMBID: r.ID,
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

	// Only confident matches are imported — the name matched AND at least one
	// library album is in the candidate's discography. Anything weaker is left
	// for the live sync, which has proper search scoring.
	best := dump.Best(candidates, libAlbums, rgAliases)

	imported, ownedTotal := 0, 0
	var unmatched []string
	for key, lib := range libByNorm {
		c := best[key]
		if c == nil {
			unmatched = append(unmatched, lib.name)
			continue
		}
		if err := st.UpsertArtist(c.Artist); err != nil {
			log.Fatalf("save artist %s: %v", c.Artist.Name, err)
		}
		if err := st.SaveReleaseGroups(c.Artist.ID, c.Groups); err != nil {
			log.Fatalf("save release groups for %s: %v", c.Artist.Name, err)
		}
		if err := st.SaveArtistAliases(c.Artist.ID, c.Artist.Name, []string{lib.name}); err != nil {
			log.Fatalf("save artist aliases for %s: %v", c.Artist.Name, err)
		}
		if len(rgAliases) > 0 {
			var rows []store.Alias
			for _, g := range c.Groups {
				for _, a := range rgAliases[g.ID] {
					rows = append(rows, store.Alias{RGID: g.ID, NormTitle: a.Norm, ReleaseMBID: a.ReleaseMBID})
					if a.Light != a.Norm {
						rows = append(rows, store.Alias{RGID: g.ID, NormTitle: a.Light, ReleaseMBID: a.ReleaseMBID})
					}
				}
			}
			if err := st.SaveAliases(c.Artist.ID, rows); err != nil {
				log.Fatalf("save aliases for %s: %v", c.Artist.Name, err)
			}
		}
		owned := 0
		for _, alb := range lib.albums {
			matched := dump.MatchGroups(c, alb, rgAliases)
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
		log.Printf("imported %s: %d release groups, %d/%d albums owned", c.Artist.Name, len(c.Groups), owned, len(lib.albums))
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

// readLibrary reads album artists from whichever player is configured. The
// MPD shape (Title + MBID per album) is the common denominator; Subsonic
// albums carry a release MBID instead of a release-group MBID, which the
// alias matcher understands; so do melody's.
func readLibrary(mpdAddr, subsonicURL, melodyAddr string) ([]mpdclient.Artist, error) {
	if mpdAddr != "" {
		host, password := mpdAddr, os.Getenv("MPD_PASSWORD")
		if pw, h, ok := strings.Cut(host, "@"); ok {
			password, host = pw, h
		}
		client, err := mpdclient.Dial(host, password)
		if err != nil {
			return nil, fmt.Errorf("mpd: %w", err)
		}
		defer client.Close()
		lib, err := client.Library()
		if err != nil {
			return nil, fmt.Errorf("mpd: %w", err)
		}
		return lib, nil
	}
	if subsonicURL == "" {
		client, err := melodyClient(melodyAddr)
		if err != nil {
			return nil, err
		}
		log.Print("reading library from melody…")
		return client.Library(context.Background())
	}
	user, pass := os.Getenv("SUBSONIC_USER"), os.Getenv("SUBSONIC_PASSWORD")
	if user == "" || pass == "" {
		return nil, fmt.Errorf("-subsonic requires SUBSONIC_USER and SUBSONIC_PASSWORD env vars")
	}
	client := subsonic.New(subsonicURL, user, pass)
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		return nil, err
	}
	log.Print("reading library from subsonic (one request per artist)…")
	ssLib, err := client.Library(ctx, func(done, total int) {
		if done%200 == 0 {
			log.Printf("  %d/%d artists", done, total)
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]mpdclient.Artist, 0, len(ssLib))
	for _, a := range ssLib {
		artist := mpdclient.Artist{Name: a.Name}
		for _, alb := range a.Albums {
			artist.Albums = append(artist.Albums, mpdclient.Album{Title: alb.Title, MBID: alb.MBID})
		}
		out = append(out, artist)
	}
	return out, nil
}

// openDump streams a MusicBrainz JSON dump line-by-line, accepting either the
// .tar.xz as downloaded or the extracted NDJSON file (much faster — no xz
// decompression). Only one record is buffered at a time; call the returned
// close func after scanning.
func openDump(path string) (*dumpScanner, func() error, error) {
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
	return &dumpScanner{reader: bufio.NewReaderSize(reader, 1<<20)}, closeFn, nil
}

// dumpScanner retains line-by-line parsing (including skipping malformed JSON
// records) without bufio.Scanner's fixed token limit. Large releases can have
// more than 32 MiB of track and relationship data in a single record.
type dumpScanner struct {
	reader *bufio.Reader
	line   []byte
	err    error
}

func (s *dumpScanner) Scan() bool {
	s.line = nil
	if s.err != nil {
		return false
	}
	s.line, s.err = s.reader.ReadBytes('\n')
	if len(s.line) == 0 {
		return false
	}
	if s.line[len(s.line)-1] == '\n' {
		s.line = s.line[:len(s.line)-1]
	}
	if len(s.line) > 0 && s.line[len(s.line)-1] == '\r' {
		s.line = s.line[:len(s.line)-1]
	}
	return true
}

func (s *dumpScanner) Bytes() []byte { return s.line }

func (s *dumpScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}

func normalizeName(name string) string { return model.NormalizeArtistName(name) }

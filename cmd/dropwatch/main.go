// Command dropwatch serves an artist-discography API and web UI for spotting
// missing albums and EPs in your music library.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"dropwatch/internal/server"
	"dropwatch/internal/sources"
	"dropwatch/internal/store"
	"dropwatch/internal/subsonic"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "import" {
		runImport(os.Args[2:])
		return
	}
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "dropwatch.db", "path to SQLite database")
	mpdAddr := flag.String("mpd", os.Getenv("MPD_HOST"), "MPD server address (host[:port]) for /api/sync/mpd; also read from MPD_HOST")
	discogsToken := flag.String("discogs-token", os.Getenv("DISCOGS_TOKEN"), "Discogs personal access token; also read from DISCOGS_TOKEN")
	subsonicURL := flag.String("subsonic", os.Getenv("SUBSONIC_URL"), "Subsonic-compatible server URL for /api/sync/subsonic; also read from SUBSONIC_URL (credentials via SUBSONIC_USER/SUBSONIC_PASSWORD)")
	flag.Parse()

	const userAgent = "dropwatch/0.1 (https://github.com/carnager/dropwatch)"

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	mb := sources.NewMusicBrainz(userAgent)

	var dc *sources.Discogs
	if *discogsToken != "" {
		dc = sources.NewDiscogs(*discogsToken, userAgent)
		log.Print("discogs source enabled")
	} else {
		log.Print("no discogs token (-discogs-token / DISCOGS_TOKEN); running with MusicBrainz only")
	}

	srv := server.New(st, mb, dc)
	if *mpdAddr != "" {
		// MPD convention: MPD_HOST may be "password@host".
		host, password := *mpdAddr, os.Getenv("MPD_PASSWORD")
		if pw, h, ok := strings.Cut(host, "@"); ok {
			password, host = pw, h
		}
		srv.SetMPD(host, password)
		log.Printf("mpd sync enabled against %s", host)
	}
	if *subsonicURL != "" {
		user, pass := os.Getenv("SUBSONIC_USER"), os.Getenv("SUBSONIC_PASSWORD")
		if user == "" || pass == "" {
			log.Fatal("-subsonic requires SUBSONIC_USER and SUBSONIC_PASSWORD env vars")
		}
		srv.SetSubsonic(subsonic.New(*subsonicURL, user, pass))
		log.Printf("subsonic sync enabled against %s", *subsonicURL)
	}
	log.Printf("dropwatch listening on %s", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

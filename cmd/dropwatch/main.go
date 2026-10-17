// Command dropwatch serves an artist-discography API and web UI for spotting
// missing albums and EPs in your music library.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"dropwatch/internal/melody"
	"dropwatch/internal/server"
	"dropwatch/internal/sources"
	"dropwatch/internal/store"
	"dropwatch/internal/subsonic"
	"dropwatch/internal/watch"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "import" {
		runImport(os.Args[2:])
		return nil
	}
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "dropwatch.db", "path to SQLite database")
	dumpDir := flag.String("dump-dir", "", "temporary dump download directory (default: <db path>.dumps)")
	mpdAddr := flag.String("mpd", os.Getenv("MPD_HOST"), "MPD server address (host[:port]) for /api/sync/mpd; also read from MPD_HOST")
	discogsToken := flag.String("discogs-token", os.Getenv("DISCOGS_TOKEN"), "Discogs personal access token; also read from DISCOGS_TOKEN")
	subsonicURL := flag.String("subsonic", os.Getenv("SUBSONIC_URL"), "Subsonic-compatible server URL for /api/sync/subsonic; also read from SUBSONIC_URL (credentials via SUBSONIC_USER/SUBSONIC_PASSWORD)")
	melodyAddr := flag.String("melody", os.Getenv("MELODY_SERVER"), "melody engine address (host[:port], default port 6603) for /api/sync/melody; also read from MELODY_SERVER (password via MELODY_PASSWORD or MELODY_PASSWORD_FILE)")
	flag.Parse()

	const userAgent = "dropwatch/0.1 (https://github.com/carnager/dropwatch)"

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
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
			return fmt.Errorf("-subsonic requires SUBSONIC_USER and SUBSONIC_PASSWORD env vars")
		}
		srv.SetSubsonic(subsonic.New(*subsonicURL, user, pass))
		log.Printf("subsonic sync enabled against %s", *subsonicURL)
	}
	if *melodyAddr != "" {
		client, err := melodyClient(*melodyAddr)
		if err != nil {
			return err
		}
		srv.SetMelody(client)
		log.Printf("melody sync enabled against %s", *melodyAddr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *dumpDir == "" {
		*dumpDir = *dbPath + ".dumps"
	}
	watcher, err := watch.New(st, *dumpDir, userAgent)
	if err != nil {
		return fmt.Errorf("start dump watcher: %w", err)
	}
	defer watcher.Close()
	srv.SetWatcher(watcher)
	log.Printf("dropwatch listening on %s", *addr)
	httpServer := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ListenAndServe() }()
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP server: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
	}
	return nil
}

// melodyClient reads the engine password from MELODY_PASSWORD or the file
// named by MELODY_PASSWORD_FILE, the variables melody's own tools use.
// A unix socket needs none.
func melodyClient(addr string) (*melody.Client, error) {
	password := os.Getenv("MELODY_PASSWORD")
	if path := os.Getenv("MELODY_PASSWORD_FILE"); password == "" && path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("melody password file: %w", err)
		}
		password = strings.TrimSpace(string(data))
	}
	if password == "" && !strings.HasPrefix(addr, "/") {
		return nil, fmt.Errorf("-melody over TCP requires MELODY_PASSWORD or MELODY_PASSWORD_FILE")
	}
	return melody.New(addr, password), nil
}

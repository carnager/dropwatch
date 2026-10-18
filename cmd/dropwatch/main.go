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
	_ "time/tzdata" // schedules use named timezones; don't depend on the host's zoneinfo

	"dropwatch/internal/melody"
	"dropwatch/internal/model"
	"dropwatch/internal/server"
	"dropwatch/internal/sources"
	"dropwatch/internal/store"
	"dropwatch/internal/watch"
)

// version is set at release build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

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
	dumpDir := flag.String("dump-dir", "", "dump download directory (default: <db path>.dumps)")
	// Everything else is set up in the web UI. These only seed the settings
	// of a new database, so existing setups carry over on upgrade.
	mpdAddr := flag.String("mpd", os.Getenv("MPD_HOST"), "first start only: MPD address (host[:port], password@host); also MPD_HOST")
	discogsToken := flag.String("discogs-token", os.Getenv("DISCOGS_TOKEN"), "first start only: Discogs token; also DISCOGS_TOKEN")
	subsonicURL := flag.String("subsonic", os.Getenv("SUBSONIC_URL"), "first start only: Subsonic URL; also SUBSONIC_URL (+ SUBSONIC_USER/SUBSONIC_PASSWORD)")
	melodyAddr := flag.String("melody", os.Getenv("MELODY_SERVER"), "first start only: melody engine address; also MELODY_SERVER (+ MELODY_PASSWORD or MELODY_PASSWORD_FILE)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("dropwatch", version)
		return nil
	}

	userAgent := "dropwatch/" + strings.TrimPrefix(version, "v") + " (https://github.com/carnager/dropwatch)"

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	if _, configured, err := st.Config(); err != nil {
		return fmt.Errorf("read settings: %w", err)
	} else if !configured {
		seed := seedConfig(*mpdAddr, *subsonicURL, *melodyAddr, *discogsToken)
		if err := st.SaveConfig(seed); err != nil {
			return fmt.Errorf("save settings: %w", err)
		}
		log.Printf("settings initialized (mpd: %v, subsonic: %v, melody: %v, discogs: %v); change them in the web UI",
			seed.MPD.Enabled, seed.Subsonic.Enabled, seed.Melody.Enabled, seed.Discogs.Token != "")
	}

	srv := server.New(st, sources.NewMusicBrainz(userAgent), userAgent)
	if err := srv.LoadConfig(); err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if set, err := st.HasAdminPassword(); err == nil && !set {
		log.Printf("no admin password yet: open the web UI to set one")
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

// seedConfig turns the legacy flags and env vars into initial settings.
// Sources that can't connect as given (missing credentials) start disabled
// but keep what was provided.
func seedConfig(mpdAddr, subsonicURL, melodyAddr, discogsToken string) model.Config {
	var c model.Config
	if mpdAddr != "" {
		// MPD convention: MPD_HOST may be "password@host".
		host, password := mpdAddr, os.Getenv("MPD_PASSWORD")
		if pw, h, ok := strings.Cut(host, "@"); ok {
			password, host = pw, h
		}
		c.MPD = model.MPDConfig{Enabled: true, Address: host, Password: password}
	}
	if subsonicURL != "" {
		c.Subsonic = model.SubsonicConfig{URL: subsonicURL, User: os.Getenv("SUBSONIC_USER"), Password: os.Getenv("SUBSONIC_PASSWORD")}
		c.Subsonic.Enabled = c.Subsonic.User != "" && c.Subsonic.Password != ""
	}
	if melodyAddr != "" {
		password, _ := melodyPassword()
		c.Melody = model.MelodyConfig{Address: melodyAddr, Password: password}
		c.Melody.Enabled = password != "" || strings.HasPrefix(melodyAddr, "/")
	}
	c.Discogs.Token = discogsToken
	return c
}

// melodyPassword reads MELODY_PASSWORD or the file named by
// MELODY_PASSWORD_FILE, the variables melody's own tools use.
func melodyPassword() (string, error) {
	password := os.Getenv("MELODY_PASSWORD")
	if path := os.Getenv("MELODY_PASSWORD_FILE"); password == "" && path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("melody password file: %w", err)
		}
		password = strings.TrimSpace(string(data))
	}
	return password, nil
}

// melodyClient reads the engine password from MELODY_PASSWORD or the file
// named by MELODY_PASSWORD_FILE, the variables melody's own tools use.
// A unix socket needs none.
func melodyClient(addr string) (*melody.Client, error) {
	password, err := melodyPassword()
	if err != nil {
		return nil, err
	}
	if password == "" && !strings.HasPrefix(addr, "/") {
		return nil, fmt.Errorf("-melody over TCP requires MELODY_PASSWORD or MELODY_PASSWORD_FILE")
	}
	return melody.New(addr, password), nil
}

package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"dropwatch/internal/melody"
	"dropwatch/internal/model"
	"dropwatch/internal/mpdclient"
	"dropwatch/internal/subsonic"
)

// librarySource is one configured player or server holding the user's music.
type librarySource struct {
	name string
	ping func(context.Context) error
	read func(ctx context.Context, progress func(string)) ([]mpdclient.Artist, error)
}

func mpdSource(c model.MPDConfig) librarySource {
	return librarySource{
		name: "mpd",
		ping: func(context.Context) error {
			client, err := mpdclient.Dial(c.Address, c.Password)
			if err != nil {
				return fmt.Errorf("mpd: %w", err)
			}
			return client.Close()
		},
		read: func(context.Context, func(string)) ([]mpdclient.Artist, error) {
			client, err := mpdclient.Dial(c.Address, c.Password)
			if err != nil {
				return nil, fmt.Errorf("mpd: %w", err)
			}
			defer client.Close()
			lib, err := client.Library()
			if err != nil {
				return nil, fmt.Errorf("mpd: %w", err)
			}
			return lib, nil
		},
	}
}

func subsonicSource(c model.SubsonicConfig) librarySource {
	client := subsonic.New(c.URL, c.User, c.Password)
	return librarySource{
		name: "subsonic",
		ping: client.Ping,
		read: func(ctx context.Context, progress func(string)) ([]mpdclient.Artist, error) {
			library, err := client.Library(ctx, func(done, total int) {
				progress(fmt.Sprintf("%d/%d artists", done, total))
			})
			if err != nil {
				return nil, err
			}
			out := make([]mpdclient.Artist, 0, len(library))
			for _, a := range library {
				artist := mpdclient.Artist{Name: a.Name}
				for _, alb := range a.Albums {
					artist.Albums = append(artist.Albums, mpdclient.Album{Title: alb.Title, MBID: alb.MBID})
				}
				out = append(out, artist)
			}
			return out, nil
		},
	}
}

func melodySource(c model.MelodyConfig) librarySource {
	client := melody.New(c.Address, c.Password)
	return librarySource{
		name: "melody",
		ping: client.Ping,
		read: func(ctx context.Context, _ func(string)) ([]mpdclient.Artist, error) { return client.Library(ctx) },
	}
}

// sourceFor builds one source by name regardless of whether it is enabled
// (for connection tests).
func sourceFor(cfg model.Config, name string) (librarySource, bool) {
	switch name {
	case "mpd":
		return mpdSource(cfg.MPD), true
	case "subsonic":
		return subsonicSource(cfg.Subsonic), true
	case "melody":
		return melodySource(cfg.Melody), true
	}
	return librarySource{}, false
}

func enabledSources(cfg model.Config) []librarySource {
	var out []librarySource
	if cfg.MPD.Enabled {
		out = append(out, mpdSource(cfg.MPD))
	}
	if cfg.Subsonic.Enabled {
		out = append(out, subsonicSource(cfg.Subsonic))
	}
	if cfg.Melody.Enabled {
		out = append(out, melodySource(cfg.Melody))
	}
	return out
}

// mergeLibraries combines several sources' libraries: artists by normalized
// name (first spelling wins), albums deduplicated by title and MBID. An album
// tagged differently in two sources is kept twice, so either tag can match.
func mergeLibraries(libraries ...[]mpdclient.Artist) []mpdclient.Artist {
	var out []mpdclient.Artist
	index := map[string]int{}
	seen := map[string]bool{}
	for _, lib := range libraries {
		for _, a := range lib {
			key := model.NormalizeArtistName(a.Name)
			if key == "" {
				key = a.Name
			}
			i, ok := index[key]
			if !ok {
				i = len(out)
				index[key] = i
				out = append(out, mpdclient.Artist{Name: a.Name})
			}
			for _, alb := range a.Albums {
				id := key + "\x00" + model.NormalizeTitle(alb.Title) + "\x00" + alb.MBID
				if seen[id] {
					continue
				}
				seen[id] = true
				out[i].Albums = append(out[i].Albums, alb)
			}
		}
	}
	return out
}

// handleSyncLibrary starts a background sync of every enabled source. It
// returns immediately; progress comes from /api/sync/status. Optional
// ?artist=<name> restricts to album artists containing the value (?exact=1:
// equal to it), and ?skip_existing=1 makes no web-service requests for
// artists already tracked.
func (s *Server) handleSyncLibrary(w http.ResponseWriter, r *http.Request) {
	srcs := enabledSources(s.config())
	if len(srcs) == 0 {
		writeErr(w, http.StatusBadRequest, "no library source is enabled; set one up in settings")
		return
	}
	s.jobMu.Lock()
	if s.job != nil && func() bool { s.job.mu.Lock(); defer s.job.mu.Unlock(); return s.job.Running }() {
		s.jobMu.Unlock()
		writeErr(w, http.StatusConflict, "a sync is already running")
		return
	}
	// Connection and password problems surface in the response instead of
	// a dead background job.
	for _, src := range srcs {
		if err := src.ping(r.Context()); err != nil {
			s.jobMu.Unlock()
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("artist")))
	exact := r.URL.Query().Get("exact") == "1"
	skipExisting := r.URL.Query().Get("skip_existing") == "1"
	job := &mpdJob{Running: true, Current: "reading library…"}
	s.job = job
	s.jobMu.Unlock()

	go func() {
		ctx := context.Background()
		var libraries [][]mpdclient.Artist
		for _, src := range srcs {
			reading := "reading library from " + src.name + "…"
			job.mu.Lock()
			job.Current = reading
			job.mu.Unlock()
			lib, err := src.read(ctx, func(detail string) {
				job.mu.Lock()
				job.Current = reading + " " + detail
				job.mu.Unlock()
			})
			if err != nil {
				log.Printf("sync: reading library from %s failed: %v", src.name, err)
				job.mu.Lock()
				job.Error = fmt.Sprintf("reading library from %s: %v", src.name, err)
				job.Running = false
				job.Current = ""
				job.mu.Unlock()
				return
			}
			libraries = append(libraries, lib)
		}
		var artists []syncArtist
		for _, a := range mergeLibraries(libraries...) {
			if filter != "" {
				n := strings.ToLower(a.Name)
				if (exact && n != filter) || (!exact && !strings.Contains(n, filter)) {
					continue
				}
			}
			in := syncArtist{Name: a.Name}
			for _, alb := range a.Albums {
				in.Albums = append(in.Albums, syncAlbum{Title: alb.Title, MBID: alb.MBID})
			}
			artists = append(artists, in)
		}
		job.mu.Lock()
		job.Total = len(artists)
		job.mu.Unlock()
		s.runSyncArtists(job, artists, skipExisting)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

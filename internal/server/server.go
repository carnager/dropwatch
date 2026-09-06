// Package server exposes the JSON API and the embedded web UI.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"dropwatch/internal/model"
	"dropwatch/internal/sources"
	"dropwatch/internal/store"
	"dropwatch/internal/subsonic"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store       *store.Store
	mb          *sources.MusicBrainz
	discogs     *sources.Discogs // nil when no token configured
	maxAge      time.Duration    // how long cached release lists stay fresh
	mpdAddr     string           // empty when no MPD server configured
	mpdPassword string

	subsonic *subsonic.Client // nil when not configured

	jobMu sync.Mutex
	job   *mpdJob // current or most recent background library sync
}

func New(st *store.Store, mb *sources.MusicBrainz, dc *sources.Discogs) *Server {
	return &Server{store: st, mb: mb, discogs: dc, maxAge: 24 * time.Hour}
}

// SetSubsonic configures the Subsonic server used by /api/sync/subsonic.
func (s *Server) SetSubsonic(c *subsonic.Client) { s.subsonic = c }

// SetMPD configures the MPD server used by /api/sync/mpd.
func (s *Server) SetMPD(addr, password string) {
	s.mpdAddr = addr
	s.mpdPassword = password
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/search/artists", s.handleSearchArtists)
	mux.HandleFunc("GET /api/artists", s.handleListArtists)
	mux.HandleFunc("GET /api/artists/{id}/releases", s.handleArtistReleases)
	mux.HandleFunc("DELETE /api/artists/{id}", s.handleDeleteArtist)
	mux.HandleFunc("GET /api/library", func(w http.ResponseWriter, r *http.Request) {
		entries, err := s.store.Library()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if entries == nil {
			entries = []model.LibraryEntry{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"library": entries})
	})
	mux.HandleFunc("PUT /api/releases/{id}/state", s.handleSetState)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	mux.HandleFunc("POST /api/sync/mpd", s.handleSyncMPD)
	mux.HandleFunc("POST /api/sync/subsonic", s.handleSyncSubsonic)
	mux.HandleFunc("GET /api/sync/status", s.handleSyncStatus)
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{
			"mpd":      s.mpdAddr != "",
			"subsonic": s.subsonic != nil,
			"discogs":  s.discogs != nil,
		})
	})
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(web))
	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleSearchArtists(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, "missing query parameter q")
		return
	}
	artists, err := s.mb.SearchArtists(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artists": artists})
}

func (s *Server) handleListArtists(w http.ResponseWriter, r *http.Request) {
	artists, err := s.store.Artists()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if artists == nil {
		artists = []model.ArtistSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artists": artists})
}

// handleArtistReleases returns the artist's grouped discography. Results are
// served from SQLite when fresh; otherwise sources are re-queried and merged.
// Query params:
//
//	types=album,ep     filter by primary type (default: album,ep)
//	missing=only       return only unowned, unignored groups
//	refresh=1          force a re-fetch from sources
func (s *Server) handleArtistReleases(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	refresh := r.URL.Query().Get("refresh") == "1"

	lastChecked, err := s.store.LastChecked(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	stale := time.Since(lastChecked) > s.maxAge

	var artist model.Artist
	if refresh || stale {
		artist, err = s.fetchAndStore(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	} else {
		artist, _, err = s.store.Artist(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	groups, err := s.store.ReleaseGroups(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	typesParam := r.URL.Query().Get("types")
	if typesParam == "" {
		typesParam = "album,ep"
	}
	var wantTypes []string
	if typesParam != "all" {
		for _, t := range strings.Split(typesParam, ",") {
			wantTypes = append(wantTypes, strings.ToLower(strings.TrimSpace(t)))
		}
	}
	missingOnly := r.URL.Query().Get("missing") == "only"

	filtered := make([]model.ReleaseGroup, 0, len(groups))
	for _, g := range groups {
		if wantTypes != nil && !slices.Contains(wantTypes, strings.ToLower(g.PrimaryType)) {
			continue
		}
		// Secondary types (Live, Compilation, Remix...) are variants, not
		// canonical studio output — exclude unless explicitly requested.
		if len(g.SecondaryTypes) > 0 && !slices.Contains(wantTypes, "variants") && typesParam != "all" {
			continue
		}
		if missingOnly && !g.Missing {
			continue
		}
		filtered = append(filtered, g)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"artist":   artist,
		"releases": filtered,
	})
}

// fetchAndStore queries all sources for the artist and persists the merged
// result. The artist must be resolvable on MusicBrainz by MBID.
func (s *Server) fetchAndStore(ctx context.Context, mbid string) (model.Artist, error) {
	stored, found, err := s.store.Artist(mbid)
	if err != nil {
		return stored, err
	}
	// Direct lookup is canonical and also yields the Discogs link. It heals
	// rows that were saved with the MBID as name by older code.
	artist, linkedDiscogsID, err := s.mb.LookupArtist(ctx, mbid)
	if err != nil {
		return stored, err
	}
	if found && stored.DiscogsID > 0 {
		artist.DiscogsID = stored.DiscogsID
	} else {
		artist.DiscogsID = linkedDiscogsID
	}
	mbGroups, err := s.mb.ReleaseGroups(ctx, mbid)
	if err != nil {
		return artist, err
	}

	var dcGroups []model.ReleaseGroup
	if s.discogs != nil {
		if artist.DiscogsID == 0 {
			did, err := s.discogs.FindArtistID(ctx, artist.Name)
			if err != nil {
				log.Printf("discogs artist search failed for %q: %v", artist.Name, err)
			}
			artist.DiscogsID = did
		}
		if artist.DiscogsID > 0 {
			dcGroups, err = s.discogs.ReleaseGroups(ctx, artist.DiscogsID)
			if err != nil {
				// Discogs being down shouldn't kill the whole request.
				log.Printf("discogs releases fetch failed for %s: %v", artist.Name, err)
			}
		}
	}

	merged := sources.Merge(mbid, mbGroups, dcGroups)
	merged = s.classifyDiscogsOnly(ctx, merged)
	if err := s.store.UpsertArtist(artist); err != nil {
		return artist, err
	}
	return artist, s.store.SaveReleaseGroups(mbid, merged)
}

func (s *Server) handleDeleteArtist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.store.DeleteArtist(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown artist")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// classifyDiscogsOnly refines groups that only Discogs knows about. Masters
// carry no release type, so these default to "Album" — which pollutes the
// album list with singles and bootlegs. Fetching each master's versions lets
// us type them properly and drop unofficial/promo-only entries.
func (s *Server) classifyDiscogsOnly(ctx context.Context, groups []model.ReleaseGroup) []model.ReleaseGroup {
	if s.discogs == nil {
		return groups
	}
	out := groups[:0]
	for _, g := range groups {
		if len(g.Sources) != 1 || g.Sources[0] != "discogs" || g.DiscogsMasterID == 0 {
			out = append(out, g)
			continue
		}
		c, err := s.discogs.ClassifyMaster(ctx, g.DiscogsMasterID)
		if err != nil {
			// Leave it as-is rather than losing the entry.
			log.Printf("classify discogs master %d (%q): %v", g.DiscogsMasterID, g.Title, err)
			out = append(out, g)
			continue
		}
		if c.Drop {
			log.Printf("dropping unofficial/promo-only discogs master %d (%q)", g.DiscogsMasterID, g.Title)
			continue
		}
		if g.PrimaryType != "EP" || c.PrimaryType != "Other" {
			g.PrimaryType = c.PrimaryType
		}
		if len(g.SecondaryTypes) == 0 {
			g.SecondaryTypes = c.SecondaryTypes
		}
		out = append(out, g)
	}
	return out
}

func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	state := model.State(body.State)
	switch state {
	case model.StateNone, model.StateOwned, model.StateIgnored:
	default:
		writeErr(w, http.StatusBadRequest, `state must be "owned", "ignored" or ""`)
		return
	}
	ok, err := s.store.SetState(id, state)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown release group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": state})
}

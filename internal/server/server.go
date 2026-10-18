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
	"dropwatch/internal/watch"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store     *store.Store
	mb        *sources.MusicBrainz
	userAgent string
	version   string        // shown in settings → about
	maxAge    time.Duration // how long cached release lists stay fresh
	watcher   *watch.Watcher

	cfgMu   sync.RWMutex
	cfg     model.Config
	discogs *sources.Discogs // nil when no token configured

	loginMu sync.Mutex // serializes password checks to slow down guessing

	jobMu sync.Mutex
	job   *mpdJob // current or most recent background library sync
}

func New(st *store.Store, mb *sources.MusicBrainz, userAgent string) *Server {
	return &Server{store: st, mb: mb, userAgent: userAgent, maxAge: 24 * time.Hour}
}

// LoadConfig applies the stored settings.
func (s *Server) LoadConfig() error {
	cfg, _, err := s.store.Config()
	if err != nil {
		return err
	}
	s.applyConfig(cfg)
	return nil
}

func (s *Server) applyConfig(cfg model.Config) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if cfg.Discogs.Token != s.cfg.Discogs.Token || (s.discogs == nil && cfg.Discogs.Token != "") {
		s.discogs = nil
		if cfg.Discogs.Token != "" {
			s.discogs = sources.NewDiscogs(cfg.Discogs.Token, s.userAgent)
		}
	}
	s.cfg = cfg
}

func (s *Server) config() model.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// dc returns the Discogs client, or nil when no token is configured.
func (s *Server) dc() *sources.Discogs {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.discogs
}

func (s *Server) SetWatcher(w *watch.Watcher) { s.watcher = w }

func (s *Server) SetVersion(v string) { s.version = v }

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
	mux.HandleFunc("POST /api/sync/library", s.handleSyncLibrary)
	mux.HandleFunc("GET /api/sync/status", s.handleSyncStatus)
	mux.HandleFunc("GET /api/dumps", s.handleDumpStatus)
	mux.HandleFunc("PUT /api/dumps/schedule", s.handleDumpSchedule)
	mux.HandleFunc("POST /api/dumps/check", s.handleDumpCheck)
	mux.HandleFunc("PUT /api/dumps/releases", s.handleReleaseSettings)
	mux.HandleFunc("POST /api/dumps/releases/check", s.handleReleaseCheck)
	mux.HandleFunc("GET /api/notifications", s.handleNotifications)
	mux.HandleFunc("POST /api/notifications/read", s.handleReadNotifications)
	mux.HandleFunc("POST /api/notifications/{id}/read", s.handleReadNotification)
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		cfg := s.config()
		writeJSON(w, http.StatusOK, map[string]any{
			"mpd":      cfg.MPD.Enabled,
			"subsonic": cfg.Subsonic.Enabled,
			"melody":   cfg.Melody.Enabled,
			"discogs":  s.dc() != nil,
			"version":  s.version,
		})
	})
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("POST /api/sources/{name}/test", s.handleTestSource)
	mux.HandleFunc("GET /api/auth", s.handleAuthState)
	mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("PUT /api/auth/password", s.handleChangePassword)
	mux.HandleFunc("POST /api/auth/token", s.handleNewAPIToken)
	mux.HandleFunc("DELETE /api/auth/token", s.handleRevokeAPIToken)
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(web))
	return logRequests(s.requireAuth(mux))
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
	// Scheduled snapshots keep tracked discographies fresh without browsing
	// the inbox triggering a costly full API refresh for every artist.
	if stale && s.watcher != nil && s.watcher.View().Schedule.Enabled {
		baseline, err := s.store.HasDumpBaseline(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		stale = !baseline
	}

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
	// Direct lookup is canonical and also yields the Discogs link and the
	// artist's registered name aliases.
	artist, nameAliases, linkedDiscogsID, err := s.mb.LookupArtist(ctx, mbid)
	if err != nil {
		return stored, err
	}
	if found && stored.DiscogsID > 0 {
		artist.DiscogsID = stored.DiscogsID
	} else {
		artist.DiscogsID = linkedDiscogsID
	}
	mbGroups, creditNames, err := s.mb.ReleaseGroups(ctx, mbid)
	if err != nil {
		return artist, err
	}
	// Credited-name forms ("Star Anna and the Laughing Dogs") plus registered
	// aliases let library tags in any of those spellings resolve to this
	// artist without a MusicBrainz search.
	if err := s.store.SaveArtistAliases(mbid, artist.Name, append(nameAliases, creditNames...)); err != nil {
		log.Printf("saving artist aliases for %s: %v", artist.Name, err)
	}

	var dcGroups []model.ReleaseGroup
	if discogs := s.dc(); discogs != nil {
		if artist.DiscogsID == 0 {
			did, err := discogs.FindArtistID(ctx, artist.Name)
			if err != nil {
				log.Printf("discogs artist search failed for %q: %v", artist.Name, err)
			}
			artist.DiscogsID = did
		}
		if artist.DiscogsID > 0 {
			dcGroups, err = discogs.ReleaseGroups(ctx, artist.DiscogsID)
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
	if err := s.store.SaveReleaseGroups(mbid, merged); err != nil {
		return artist, err
	}

	// Release titles differ from their group titles (reissues, regional
	// names). Storing them as aliases lets syncs match a library album
	// against any version of a release group.
	releases, err := s.mb.ReleaseAliases(ctx, mbid)
	if err != nil {
		log.Printf("release alias fetch failed for %s: %v", artist.Name, err)
		return artist, nil
	}
	known := make(map[string]bool, len(merged))
	for _, g := range merged {
		known[g.ID] = true
	}
	var aliases []store.Alias
	for _, r := range releases {
		if !known[r.ReleaseGroupID] {
			continue
		}
		// Store both normalizations: the stripped one collapses editions,
		// the light one preserves the exact release title for tags that
		// carry it verbatim in any punctuation variant.
		stripped := model.NormalizeTitle(r.Title)
		light := model.NormalizeTitleLight(r.Title)
		aliases = append(aliases, store.Alias{
			RGID:        r.ReleaseGroupID,
			NormTitle:   stripped,
			ReleaseMBID: r.ReleaseMBID,
		})
		if light != stripped {
			aliases = append(aliases, store.Alias{
				RGID:        r.ReleaseGroupID,
				NormTitle:   light,
				ReleaseMBID: r.ReleaseMBID,
			})
		}
	}
	if err := s.store.SaveAliases(mbid, aliases); err != nil {
		return artist, err
	}
	return artist, nil
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
	discogs := s.dc()
	if discogs == nil {
		return groups
	}
	out := groups[:0]
	for _, g := range groups {
		if len(g.Sources) != 1 || g.Sources[0] != "discogs" || g.DiscogsMasterID == 0 {
			out = append(out, g)
			continue
		}
		c, err := discogs.ClassifyMaster(ctx, g.DiscogsMasterID)
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

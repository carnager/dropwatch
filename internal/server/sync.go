package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"dropwatch/internal/model"
	"dropwatch/internal/mpdclient"
)

// syncAlbum is one album from the user's library: either a bare title string
// or {"title": ..., "mbid": ...} where mbid is the MusicBrainz release-group
// ID (the MUSICBRAINZ_RELEASEGROUPID tag). An MBID match is exact and beats
// title normalization.
type syncAlbum struct {
	Title string `json:"title"`
	MBID  string `json:"mbid,omitempty"`
}

func (a *syncAlbum) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &a.Title)
	}
	type raw syncAlbum
	return json.Unmarshal(b, (*raw)(a))
}

// syncArtist is one artist from the user's local library.
type syncArtist struct {
	Name   string      `json:"name"`
	MBID   string      `json:"mbid,omitempty"` // skips name resolution when given
	Albums []syncAlbum `json:"albums"`
}

type syncResult struct {
	Query     string               `json:"query"`
	Artist    *model.Artist        `json:"artist,omitempty"`
	Error     string               `json:"error,omitempty"`
	Skipped   bool                 `json:"skipped,omitempty"`
	Owned     int                  `json:"owned"`
	Unmatched []string             `json:"unmatched,omitempty"`
	Missing   []model.ReleaseGroup `json:"missing"`
}

// handleSync takes the user's library ({artist, albums[]} pairs), marks every
// matched release group as owned, and reports what's missing per artist.
// Artists not seen before are fetched from the sources first, so a first sync
// of a large library is slow (rate limits) — subsequent syncs hit the cache.
//
//	POST /api/sync
//	{"artists": [{"name": "Boards of Canada", "albums": ["Geogaddi", ...]}]}
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Artists      []syncArtist `json:"artists"`
		SkipExisting bool         `json:"skipExisting"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Artists) == 0 {
		writeErr(w, http.StatusBadRequest, "artists list is empty")
		return
	}

	results := make([]syncResult, 0, len(req.Artists))
	for _, in := range req.Artists {
		results = append(results, s.syncOne(r.Context(), in, req.SkipExisting))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// syncOne resolves one library artist, marks its albums owned, and reports
// what's missing. With skipExisting, artists already tracked in the database
// are skipped entirely — no source requests at all — which makes re-syncs of
// large libraries fast.
func (s *Server) syncOne(ctx context.Context, in syncArtist, skipExisting bool) syncResult {
	res := syncResult{Query: in.Name, Missing: []model.ReleaseGroup{}}

	if skipExisting {
		var existing model.Artist
		var found bool
		var err error
		if in.MBID != "" {
			existing, found, err = s.store.Artist(in.MBID)
		} else {
			existing, found, err = s.store.ArtistByName(in.Name)
		}
		if err == nil && found {
			res.Artist = &existing
			res.Skipped = true
			return res
		}
	}

	mbid := in.MBID
	if mbid == "" {
		found, err := s.mb.SearchArtists(ctx, in.Name)
		if err != nil {
			res.Error = "artist search failed: " + err.Error()
			return res
		}
		if len(found) == 0 {
			res.Error = "no MusicBrainz match for artist name"
			return res
		}
		mbid = found[0].ID
	}

	lastChecked, err := s.store.LastChecked(mbid)
	if err == nil && lastChecked.IsZero() {
		if _, err := s.fetchAndStore(ctx, mbid); err != nil {
			res.Error = "fetching releases failed: " + err.Error()
			return res
		}
	}
	artist, _, err := s.store.Artist(mbid)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Artist = &artist

	groups, err := s.store.ReleaseGroups(mbid)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	byNorm := make(map[string]*model.ReleaseGroup, len(groups))
	byMBID := make(map[string]*model.ReleaseGroup, len(groups))
	for i := range groups {
		byNorm[groups[i].NormTitle] = &groups[i]
		if groups[i].MBID != "" {
			byMBID[groups[i].MBID] = &groups[i]
		}
	}

	for _, album := range in.Albums {
		g, ok := byMBID[album.MBID]
		if !ok {
			g, ok = byNorm[model.NormalizeTitle(album.Title)]
		}
		if !ok {
			label := album.Title
			if label == "" {
				label = album.MBID
			}
			res.Unmatched = append(res.Unmatched, label)
			continue
		}
		if g.State != model.StateOwned {
			if _, err := s.store.SetState(g.ID, model.StateOwned); err != nil {
				log.Printf("sync: set owned %s: %v", g.ID, err)
				continue
			}
			g.State = model.StateOwned
			g.Missing = false
		}
		res.Owned++
	}

	for _, g := range groups {
		t := strings.ToLower(g.PrimaryType)
		if g.Missing && (t == "album" || t == "ep") && len(g.SecondaryTypes) == 0 {
			res.Missing = append(res.Missing, g)
		}
	}
	return res
}

// mpdJob tracks a background MPD sync. A first sync of a large library takes
// far longer than any HTTP request should live, so the work runs detached
// from the request context and the UI polls /api/sync/status.
type mpdJob struct {
	mu        sync.Mutex
	Running   bool
	Total     int
	Processed int
	Current   string
	Owned     int
	Missing   int
	Skipped   int
	Failed    int
	Unmatched int
	Error     string // fatal error that aborted the job
}

func (j *mpdJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"running": j.Running, "total": j.Total, "processed": j.Processed,
		"current": j.Current, "owned": j.Owned, "missing": j.Missing,
		"skipped": j.Skipped, "failed": j.Failed, "unmatched": j.Unmatched,
		"error": j.Error,
	}
}

// handleSyncMPD starts a background pull sync from the configured MPD server.
// It returns immediately; progress comes from /api/sync/status. Optional
// ?artist=<name> restricts to album artists containing the value, and
// ?skip_existing=1 skips artists already tracked.
func (s *Server) handleSyncMPD(w http.ResponseWriter, r *http.Request) {
	if s.mpdAddr == "" {
		writeErr(w, http.StatusBadRequest, "no MPD server configured (start dropwatch with -mpd or MPD_HOST)")
		return
	}
	s.jobMu.Lock()
	if s.job != nil && func() bool { s.job.mu.Lock(); defer s.job.mu.Unlock(); return s.job.Running }() {
		s.jobMu.Unlock()
		writeErr(w, http.StatusConflict, "a sync is already running")
		return
	}

	// Read the library synchronously so connection problems surface in the
	// response instead of a dead background job.
	client, err := mpdclient.Dial(s.mpdAddr, s.mpdPassword)
	if err != nil {
		s.jobMu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	library, err := client.Library()
	client.Close()
	if err != nil {
		s.jobMu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("artist")))
	skipExisting := r.URL.Query().Get("skip_existing") == "1"
	var artists []syncArtist
	for _, a := range library {
		if filter != "" && !strings.Contains(strings.ToLower(a.Name), filter) {
			continue
		}
		in := syncArtist{Name: a.Name}
		for _, alb := range a.Albums {
			in.Albums = append(in.Albums, syncAlbum{Title: alb.Title, MBID: alb.MBID})
		}
		artists = append(artists, in)
	}

	job := &mpdJob{Running: true, Total: len(artists)}
	s.job = job
	s.jobMu.Unlock()

	go s.runSyncArtists(job, artists, skipExisting)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "total": len(artists)})
}

// runSyncArtists is the shared per-artist loop for all background library
// syncs (MPD, Subsonic, ...).
func (s *Server) runSyncArtists(job *mpdJob, artists []syncArtist, skipExisting bool) {
	// Deliberately not the request context: the sync must survive the browser
	// giving up on a long request.
	ctx := context.Background()
	log.Printf("sync: starting, %d artists", len(artists))
	for _, in := range artists {
		job.mu.Lock()
		job.Current = in.Name
		job.mu.Unlock()

		res := s.syncOne(ctx, in, skipExisting)

		job.mu.Lock()
		job.Processed++
		job.Owned += res.Owned
		job.Missing += len(res.Missing)
		job.Unmatched += len(res.Unmatched)
		switch {
		case res.Skipped:
			job.Skipped++
		case res.Error != "":
			job.Failed++
		}
		job.mu.Unlock()

		switch {
		case res.Skipped:
		case res.Error != "":
			log.Printf("sync: %s — FAILED: %s", in.Name, res.Error)
		case len(res.Unmatched) > 0:
			log.Printf("sync: %s — owned %d, missing %d, unmatched: %s",
				in.Name, res.Owned, len(res.Missing), strings.Join(res.Unmatched, "; "))
		default:
			log.Printf("sync: %s — owned %d, missing %d", in.Name, res.Owned, len(res.Missing))
		}
	}
	job.mu.Lock()
	job.Running = false
	job.Current = ""
	job.mu.Unlock()
	log.Printf("sync: finished %d artists", len(artists))
}

// handleSyncSubsonic starts a background pull sync from the configured
// Subsonic-compatible server. Unlike MPD, reading the library is one request
// per artist, so it happens inside the job; a synchronous ping still surfaces
// connection/auth errors in the response. Same query params as /api/sync/mpd.
func (s *Server) handleSyncSubsonic(w http.ResponseWriter, r *http.Request) {
	if s.subsonic == nil {
		writeErr(w, http.StatusBadRequest, "no Subsonic server configured (start dropwatch with -subsonic + SUBSONIC_USER/SUBSONIC_PASSWORD)")
		return
	}
	s.jobMu.Lock()
	if s.job != nil && func() bool { s.job.mu.Lock(); defer s.job.mu.Unlock(); return s.job.Running }() {
		s.jobMu.Unlock()
		writeErr(w, http.StatusConflict, "a sync is already running")
		return
	}
	if err := s.subsonic.Ping(r.Context()); err != nil {
		s.jobMu.Unlock()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("artist")))
	skipExisting := r.URL.Query().Get("skip_existing") == "1"
	job := &mpdJob{Running: true, Current: "reading library from subsonic…"}
	s.job = job
	s.jobMu.Unlock()

	go func() {
		ctx := context.Background()
		library, err := s.subsonic.Library(ctx, func(done, total int) {
			job.mu.Lock()
			job.Current = fmt.Sprintf("reading library from subsonic… %d/%d artists", done, total)
			job.mu.Unlock()
		})
		if err != nil {
			log.Printf("subsonic sync: reading library failed: %v", err)
			job.mu.Lock()
			job.Error = err.Error()
			job.Running = false
			job.Current = ""
			job.mu.Unlock()
			return
		}
		var artists []syncArtist
		for _, a := range library {
			if filter != "" && !strings.Contains(strings.ToLower(a.Name), filter) {
				continue
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

// handleSyncStatus reports the state of the current or last MPD sync job.
func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	s.jobMu.Lock()
	job := s.job
	s.jobMu.Unlock()
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"idle": true})
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

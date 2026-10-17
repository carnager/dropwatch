package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"dropwatch/internal/dump"
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

	resolved bool // resolved earlier in this sync (dump, lead artist): no skip, no live refetch
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
// cause no source requests at all — their albums are matched against the
// stored discography only — which makes re-syncs of large libraries fast.
func (s *Server) syncOne(ctx context.Context, in syncArtist, skipExisting bool) syncResult {
	res := syncResult{Query: in.Name, Missing: []model.ReleaseGroup{}}

	if skipExisting && !in.resolved {
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
			if err := s.matchAlbums(existing.ID, in.Albums, &res); err != nil {
				res.Error = err.Error()
			}
			return res
		}
	}

	// Resolve locally first: a tracked artist by that exact name needs no
	// MusicBrainz search, which makes full (non-skip) re-syncs of a large
	// library nearly free in API calls.
	mbid := in.MBID
	if mbid == "" {
		if a, found, err := s.store.ArtistByName(in.Name); err == nil && found {
			mbid = a.ID
		}
	}
	if mbid == "" {
		mbid = s.leadArtist(in)
	}
	if mbid == "" {
		// A tagged album names its artist exactly: one lookup, no guessing.
		// Several are tried in case a tag is stale or points at a compilation.
		tried := 0
		for _, alb := range in.Albums {
			if alb.MBID == "" || tried == 3 {
				continue
			}
			tried++
			a, ok, err := s.mb.CreditedArtist(ctx, alb.MBID)
			if err != nil {
				log.Printf("sync: %s: credit lookup for %q: %v", in.Name, alb.Title, err)
				continue
			}
			if ok {
				mbid = a.ID
				break
			}
		}
	}
	if mbid == "" {
		// Only a hit actually named like the library artist counts: the top
		// fuzzy result for an artist MusicBrainz doesn't know is somebody else.
		// A joint credit no artist goes by falls back to its first artist.
		for _, name := range []string{in.Name, model.LeadArtist(in.Name)} {
			if name == "" || mbid != "" {
				continue
			}
			found, err := s.mb.SearchArtists(ctx, name)
			if err != nil {
				res.Error = "artist search failed: " + err.Error()
				return res
			}
			for _, a := range found {
				if a.ID != model.VariousArtistsMBID && a.ID != model.UnknownArtistMBID && a.NamedAs(name) {
					mbid = a.ID
					break
				}
			}
		}
		if mbid == "" {
			res.Error = "no MusicBrainz artist by that name"
			return res
		}
	}

	fetchedNow := false
	lastChecked, err := s.store.LastChecked(mbid)
	if err == nil && lastChecked.IsZero() {
		if _, err := s.fetchAndStore(ctx, mbid); err != nil {
			res.Error = "fetching releases failed: " + err.Error()
			return res
		}
		fetchedNow = true
	}
	artist, _, err := s.store.Artist(mbid)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Artist = &artist

	if err := s.matchAlbums(mbid, in.Albums, &res); err != nil {
		res.Error = err.Error()
		return res
	}
	// Self-heal: cached data can predate features (release aliases) or be
	// stale. If albums failed to match against the cache, refetch once and
	// retry before reporting them unmatched.
	if len(res.Unmatched) > 0 && !fetchedNow && !in.resolved {
		log.Printf("sync: %s — %d unmatched against cache, refetching sources", in.Name, len(res.Unmatched))
		if _, err := s.fetchAndStore(ctx, mbid); err != nil {
			log.Printf("sync: refetch for %s failed: %v", in.Name, err)
			return res
		}
		res.Owned, res.Unmatched, res.Missing = 0, nil, []model.ReleaseGroup{}
		if err := s.matchAlbums(mbid, in.Albums, &res); err != nil {
			res.Error = err.Error()
		}
	}
	return res
}

// leadArtist resolves a joint credit ("Dear Reader & Deutsches Filmorchester
// Babelsberg") to its tracked first artist, but only when one of the library's
// albums is in that artist's discography: "Nick Cave & The Bad Seeds" must not
// collapse into Nick Cave's solo work.
func (s *Server) leadArtist(in syncArtist) string {
	lead := model.LeadArtist(in.Name)
	if lead == "" {
		return ""
	}
	a, found, err := s.store.ArtistByName(lead)
	if err != nil || !found {
		return ""
	}
	groups, err := s.store.ReleaseGroups(a.ID)
	if err != nil {
		return ""
	}
	c := &dump.Candidate{Artist: a, Groups: groups}
	aliases := map[string][]dump.Alias{}
	if stored, err := s.store.Aliases(a.ID); err == nil {
		for _, al := range stored {
			aliases[al.RGID] = append(aliases[al.RGID], dump.Alias{Norm: al.NormTitle, Light: al.NormTitle, ReleaseMBID: al.ReleaseMBID})
		}
	}
	for _, alb := range in.Albums {
		if len(dump.MatchGroups(c, mpdclient.Album{Title: alb.Title, MBID: alb.MBID}, aliases)) > 0 {
			if err := s.store.AddArtistAlias(a.ID, in.Name); err != nil {
				log.Printf("sync: alias for %s: %v", a.Name, err)
			}
			return a.ID
		}
	}
	return ""
}

// matchAlbums marks the library's albums owned against the artist's stored
// release groups and fills in owned/unmatched/missing on res.
func (s *Server) matchAlbums(mbid string, albums []syncAlbum, res *syncResult) error {
	groups, err := s.store.ReleaseGroups(mbid)
	if err != nil {
		return err
	}
	byNorm := make(map[string][]*model.ReleaseGroup, len(groups))
	byMBID := make(map[string]*model.ReleaseGroup, len(groups))
	byID := make(map[string]*model.ReleaseGroup, len(groups))
	for i := range groups {
		byNorm[groups[i].NormTitle] = append(byNorm[groups[i].NormTitle], &groups[i])
		byID[groups[i].ID] = &groups[i]
		if groups[i].MBID != "" {
			byMBID[groups[i].MBID] = &groups[i]
		}
	}
	// Release-title aliases: a library album named after any *version* of a
	// release group ("Aaliyah: Edition 2004") matches the group itself.
	aliasNorm := map[string][]*model.ReleaseGroup{}
	aliasRelMBID := map[string]*model.ReleaseGroup{}
	if aliases, err := s.store.Aliases(mbid); err == nil {
		for _, a := range aliases {
			g, ok := byID[a.RGID]
			if !ok {
				continue
			}
			aliasNorm[a.NormTitle] = append(aliasNorm[a.NormTitle], g)
			if a.ReleaseMBID != "" {
				aliasRelMBID[a.ReleaseMBID] = g
			}
		}
	}

	for _, album := range albums {
		norm := model.NormalizeTitle(album.Title)
		var matched []*model.ReleaseGroup
		if g, ok := byMBID[album.MBID]; ok { // release-group MBID tag
			matched = []*model.ReleaseGroup{g}
		} else if g, ok := aliasRelMBID[album.MBID]; ok { // release MBID (Subsonic)
			matched = []*model.ReleaseGroup{g}
		} else if gs, ok := byNorm[norm]; ok {
			matched = gs
		} else if gs, ok := aliasNorm[norm]; ok {
			matched = gs
		} else if gs, ok := aliasNorm[model.NormalizeTitleLight(album.Title)]; ok {
			matched = gs
		}
		if len(matched) == 0 {
			label := album.Title
			if label == "" {
				label = album.MBID
			}
			res.Unmatched = append(res.Unmatched, label)
			continue
		}
		for _, g := range model.PreferCanonical(matched) {
			if g.State == model.StateOwned {
				continue
			}
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
	return nil
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

// runSyncArtists is the per-artist loop of a background library sync.
func (s *Server) runSyncArtists(job *mpdJob, artists []syncArtist, skipExisting bool) {
	// Deliberately not the request context: the sync must survive the browser
	// giving up on a long request.
	ctx := context.Background()
	log.Printf("sync: starting, %d artists", len(artists))
	s.resolveFromDump(ctx, job, artists)
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

// resolveFromDump looks up every library artist the database doesn't know in
// one scan of the locally kept dump, instead of a few rate-limited web-service
// requests each. Confident matches (name plus at least one album) are saved
// and get their MBID filled in; the rest fall through to the live search.
func (s *Server) resolveFromDump(ctx context.Context, job *mpdJob, artists []syncArtist) {
	if s.watcher == nil {
		return
	}
	path := s.watcher.DumpPath()
	names := map[string]bool{}
	albums := map[string][]mpdclient.Album{}
	for i, in := range artists {
		key := model.NormalizeArtistName(in.Name)
		if in.MBID != "" || key == "" || key == "various artists" || key == "va" {
			continue
		}
		if _, found, err := s.store.ArtistByName(in.Name); err != nil || found {
			continue
		}
		if id := s.leadArtist(in); id != "" {
			artists[i].MBID, artists[i].resolved = id, true
			continue
		}
		names[key] = true
		for _, a := range in.Albums {
			albums[key] = append(albums[key], mpdclient.Album{Title: a.Title, MBID: a.MBID})
		}
	}
	if len(names) == 0 {
		return
	}
	if path == "" {
		log.Printf("sync: %d new artists, no local dump kept yet; using the web service (run a dump check to get one)", len(names))
		return
	}
	// Names this very dump already failed on would only repeat a full scan.
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	var misses struct {
		Dump  string          `json:"dump"`
		Names map[string]bool `json:"names"`
	}
	stamp := info.ModTime().UTC().Format(time.RFC3339Nano)
	if err := s.store.LoadDumpSetting("unresolved", &misses); err != nil || misses.Dump != stamp || misses.Names == nil {
		misses.Dump, misses.Names = stamp, map[string]bool{}
	}
	for key := range names {
		if misses.Names[key] {
			delete(names, key)
		}
	}
	if len(names) == 0 {
		return
	}
	status := fmt.Sprintf("resolving %d new artists from the local dump…", len(names))
	job.mu.Lock()
	job.Current = status
	job.mu.Unlock()
	log.Printf("sync: %s", status)
	cands, err := dump.FindCandidates(ctx, path, names, func(records int) {
		job.mu.Lock()
		job.Current = fmt.Sprintf("%s %dk release groups scanned", status, records/1000)
		job.mu.Unlock()
	})
	if err != nil {
		log.Printf("sync: dump lookup failed, using the web service: %v", err)
		return
	}
	best := dump.Best(cands, albums, nil)
	for key := range names {
		if best[key] == nil {
			misses.Names[key] = true
		}
	}
	if err := s.store.SaveDumpSetting("unresolved", misses); err != nil {
		log.Printf("sync: %v", err)
	}
	for i := range artists {
		in := &artists[i]
		c := best[model.NormalizeArtistName(in.Name)]
		if in.MBID != "" || c == nil {
			continue
		}
		// A joint credit ("Beth Gibbons & Rustin Man") can resolve to an artist
		// already tracked; its stored discography is fuller than the dump's
		// slice of it, so only the new name is recorded.
		_, tracked, err := s.store.Artist(c.Artist.ID)
		if err != nil {
			log.Printf("sync: %s: %v", c.Artist.Name, err)
			continue
		}
		if !tracked {
			if err := s.store.UpsertArtist(c.Artist); err != nil {
				log.Printf("sync: saving %s: %v", c.Artist.Name, err)
				continue
			}
			if err := s.store.SaveReleaseGroups(c.Artist.ID, c.Groups); err != nil {
				log.Printf("sync: saving releases for %s: %v", c.Artist.Name, err)
				continue
			}
		}
		if err := s.store.AddArtistAlias(c.Artist.ID, in.Name); err != nil {
			log.Printf("sync: saving aliases for %s: %v", c.Artist.Name, err)
		}
		in.MBID, in.resolved = c.Artist.ID, true
	}
	log.Printf("sync: %d/%d new artists resolved from the dump", len(best), len(names))
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

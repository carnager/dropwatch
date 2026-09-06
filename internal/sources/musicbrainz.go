package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dropwatch/internal/model"
)

// MusicBrainz is the primary source. It is rate-limited to ~1 request/second
// per their API policy and requires a meaningful User-Agent.
type MusicBrainz struct {
	client    *http.Client
	userAgent string
	throttle  <-chan time.Time
}

func NewMusicBrainz(userAgent string) *MusicBrainz {
	return &MusicBrainz{
		client:    &http.Client{Timeout: 15 * time.Second},
		userAgent: userAgent,
		throttle:  time.Tick(1100 * time.Millisecond),
	}
}

func (m *MusicBrainz) get(ctx context.Context, path string, params url.Values, out any) error {
	params.Set("fmt", "json")
	u := "https://musicbrainz.org/ws/2/" + path + "?" + params.Encode()
	// MusicBrainz returns 503 when it sheds load, even for clients that stay
	// under 1 req/s. Back off and retry so bulk syncs don't fall over.
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		select {
		case <-m.throttle:
		case <-ctx.Done():
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", m.userAgent)
		resp, err := m.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			lastErr = fmt.Errorf("musicbrainz: %s returned %s", path, resp.Status)
			select {
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("musicbrainz: %s returned %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return lastErr
}

// SearchArtists finds artists by name.
func (m *MusicBrainz) SearchArtists(ctx context.Context, query string) ([]model.Artist, error) {
	var res struct {
		Artists []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			SortName       string `json:"sort-name"`
			Disambiguation string `json:"disambiguation"`
			Country        string `json:"country"`
			Score          int    `json:"score"`
		} `json:"artists"`
	}
	params := url.Values{"query": {query}, "limit": {"10"}}
	if err := m.get(ctx, "artist", params, &res); err != nil {
		return nil, err
	}
	artists := make([]model.Artist, 0, len(res.Artists))
	for _, a := range res.Artists {
		artists = append(artists, model.Artist{
			ID:             a.ID,
			Name:           a.Name,
			SortName:       a.SortName,
			Disambiguation: a.Disambiguation,
			Country:        a.Country,
		})
	}
	return artists, nil
}

// ReleaseGroups fetches all release groups for an artist MBID, paging as needed.
func (m *MusicBrainz) ReleaseGroups(ctx context.Context, artistMBID string) ([]model.ReleaseGroup, error) {
	const pageSize = 100
	var groups []model.ReleaseGroup
	for offset := 0; ; offset += pageSize {
		var res struct {
			Count         int `json:"release-group-count"`
			ReleaseGroups []struct {
				ID               string   `json:"id"`
				Title            string   `json:"title"`
				PrimaryType      string   `json:"primary-type"`
				SecondaryTypes   []string `json:"secondary-types"`
				FirstReleaseDate string   `json:"first-release-date"`
			} `json:"release-groups"`
		}
		params := url.Values{
			"artist": {artistMBID},
			"limit":  {fmt.Sprint(pageSize)},
			"offset": {fmt.Sprint(offset)},
		}
		if err := m.get(ctx, "release-group", params, &res); err != nil {
			return nil, err
		}
		for _, rg := range res.ReleaseGroups {
			pt := rg.PrimaryType
			if pt == "" {
				pt = "Other"
			}
			groups = append(groups, model.ReleaseGroup{
				ID:               rg.ID,
				ArtistID:         artistMBID,
				Title:            rg.Title,
				NormTitle:        model.NormalizeTitle(rg.Title),
				PrimaryType:      pt,
				SecondaryTypes:   rg.SecondaryTypes,
				FirstReleaseDate: rg.FirstReleaseDate,
				MBID:             rg.ID,
				Sources:          []string{"musicbrainz"},
			})
		}
		if offset+pageSize >= res.Count || len(res.ReleaseGroups) == 0 {
			break
		}
	}
	return groups, nil
}

// LookupArtist fetches an artist directly by MBID (canonical, unlike search,
// which depends on the search indexer). It also returns the linked Discogs
// artist ID from the URL relations, or 0 when none is linked.
func (m *MusicBrainz) LookupArtist(ctx context.Context, artistMBID string) (model.Artist, int64, error) {
	var res struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		SortName       string `json:"sort-name"`
		Disambiguation string `json:"disambiguation"`
		Country        string `json:"country"`
		Relations      []struct {
			Type string `json:"type"`
			URL  struct {
				Resource string `json:"resource"`
			} `json:"url"`
		} `json:"relations"`
	}
	params := url.Values{"inc": {"url-rels"}}
	if err := m.get(ctx, "artist/"+artistMBID, params, &res); err != nil {
		return model.Artist{}, 0, err
	}
	artist := model.Artist{
		ID:             res.ID,
		Name:           res.Name,
		SortName:       res.SortName,
		Disambiguation: res.Disambiguation,
		Country:        res.Country,
	}
	for _, rel := range res.Relations {
		if rel.Type != "discogs" {
			continue
		}
		// e.g. https://www.discogs.com/artist/125246
		parts := strings.Split(strings.TrimRight(rel.URL.Resource, "/"), "/")
		var id int64
		if _, err := fmt.Sscanf(parts[len(parts)-1], "%d", &id); err == nil && id > 0 {
			return artist, id, nil
		}
	}
	return artist, 0, nil
}

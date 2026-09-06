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

// Discogs is an optional secondary source, enabled when a personal access
// token is configured. Authenticated requests are limited to 60/minute.
type Discogs struct {
	client    *http.Client
	token     string
	userAgent string
	throttle  <-chan time.Time
}

func NewDiscogs(token, userAgent string) *Discogs {
	return &Discogs{
		client:    &http.Client{Timeout: 15 * time.Second},
		token:     token,
		userAgent: userAgent,
		throttle:  time.Tick(1100 * time.Millisecond),
	}
}

func (d *Discogs) get(ctx context.Context, path string, params url.Values, out any) error {
	u := "https://api.discogs.com" + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		select {
		case <-d.throttle:
		case <-ctx.Done():
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", d.userAgent)
		req.Header.Set("Authorization", "Discogs token="+d.token)
		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			lastErr = fmt.Errorf("discogs: %s returned %s", path, resp.Status)
			select {
			case <-time.After(time.Duration(attempt+1) * 5 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("discogs: %s returned %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return lastErr
}

// FindArtistID searches Discogs for an artist by name and returns the best
// match. Used only as a fallback when MusicBrainz has no Discogs URL relation.
func (d *Discogs) FindArtistID(ctx context.Context, name string) (int64, error) {
	var res struct {
		Results []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"results"`
	}
	params := url.Values{"type": {"artist"}, "q": {name}, "per_page": {"5"}}
	if err := d.get(ctx, "/database/search", params, &res); err != nil {
		return 0, err
	}
	if len(res.Results) == 0 {
		return 0, nil
	}
	return res.Results[0].ID, nil
}

// discogsRelease is one row of an artist's release list.
type discogsRelease struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Type   string `json:"type"` // "master" or "release"
	Role   string `json:"role"` // "Main", "Appearance", ...
	Year   int    `json:"year"`
	Format string `json:"format"`
}

// Classification is the inferred release type of a Discogs master, derived
// from the formats of its versions (masters themselves carry no type).
type Classification struct {
	PrimaryType    string   // Album, EP, Single or Other
	SecondaryTypes []string // e.g. Compilation
	Drop           bool     // every version is unofficial or promo-only
}

// ClassifyMaster inspects a master's versions and infers what kind of release
// it is. Bootlegs and promo-only masters are flagged for dropping.
func (d *Discogs) ClassifyMaster(ctx context.Context, masterID int64) (Classification, error) {
	var res struct {
		Versions []struct {
			Format string `json:"format"`
		} `json:"versions"`
	}
	params := url.Values{"per_page": {"50"}}
	err := d.get(ctx, fmt.Sprintf("/masters/%d/versions", masterID), params, &res)
	if err != nil {
		return Classification{}, err
	}
	var c Classification
	official := false
	for _, v := range res.Versions {
		tokens := map[string]bool{}
		for _, t := range strings.Split(v.Format, ",") {
			tokens[strings.ToLower(strings.TrimSpace(t))] = true
		}
		if !tokens["unofficial release"] && !tokens["promo"] {
			official = true
		}
		switch {
		case tokens["album"], tokens["lp"]:
			c.PrimaryType = "Album"
		case tokens["ep"], tokens["mini-album"]:
			if c.PrimaryType != "Album" {
				c.PrimaryType = "EP"
			}
		case tokens["single"], tokens["maxi-single"], tokens[`7"`]:
			if c.PrimaryType == "" {
				c.PrimaryType = "Single"
			}
		}
		if tokens["compilation"] || tokens["box set"] {
			c.SecondaryTypes = []string{"Compilation"}
		}
	}
	if c.PrimaryType == "" {
		c.PrimaryType = "Other"
	}
	c.Drop = !official && len(res.Versions) > 0
	return c, nil
}

// ReleaseGroups fetches the artist's main releases and returns them as
// release groups. Only "master" entries with role "Main" are used: masters are
// Discogs' own version-grouping, so each one maps to a logical album.
func (d *Discogs) ReleaseGroups(ctx context.Context, artistID int64) ([]model.ReleaseGroup, error) {
	const pageSize = 100
	var groups []model.ReleaseGroup
	for page := 1; ; page++ {
		var res struct {
			Pagination struct {
				Pages int `json:"pages"`
			} `json:"pagination"`
			Releases []discogsRelease `json:"releases"`
		}
		params := url.Values{
			"sort":     {"year"},
			"per_page": {fmt.Sprint(pageSize)},
			"page":     {fmt.Sprint(page)},
		}
		err := d.get(ctx, fmt.Sprintf("/artists/%d/releases", artistID), params, &res)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Releases {
			if r.Type != "master" || r.Role != "Main" {
				continue
			}
			date := ""
			if r.Year > 0 {
				date = fmt.Sprint(r.Year)
			}
			groups = append(groups, model.ReleaseGroup{
				ID:               fmt.Sprintf("discogs-%d", r.ID),
				Title:            r.Title,
				NormTitle:        model.NormalizeTitle(r.Title),
				PrimaryType:      "Album", // Discogs masters carry no type; refined during merge
				FirstReleaseDate: date,
				DiscogsMasterID:  r.ID,
				Sources:          []string{"discogs"},
			})
		}
		if page >= res.Pagination.Pages {
			break
		}
	}
	return groups, nil
}

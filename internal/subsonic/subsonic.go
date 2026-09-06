// Package subsonic reads the library from a Subsonic-compatible server
// (Navidrome, gonic, Airsonic, ...) via the ID3 browsing endpoints, which
// operate on album artists.
package subsonic

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Album struct {
	Title string
	MBID  string // release MBID on OpenSubsonic servers; may not match a release group
}

type Artist struct {
	Name   string
	Albums []Album
}

type Client struct {
	base     string
	user     string
	password string
	http     *http.Client
}

func New(baseURL, user, password string) *Client {
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

type response struct {
	Resp struct {
		Status string `json:"status"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Artists *struct {
			Index []struct {
				Artist []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"artist"`
			} `json:"index"`
		} `json:"artists"`
		Artist *struct {
			Album []struct {
				Name          string `json:"name"`
				MusicBrainzID string `json:"musicBrainzId"`
			} `json:"album"`
		} `json:"artist"`
	} `json:"subsonic-response"`
}

func (c *Client) call(ctx context.Context, endpoint string, params url.Values) (*response, error) {
	// Salted token auth per the Subsonic API spec; the password itself never
	// goes over the wire.
	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		return nil, err
	}
	salt := hex.EncodeToString(saltBytes)
	token := md5.Sum([]byte(c.password + salt))
	params.Set("u", c.user)
	params.Set("t", hex.EncodeToString(token[:]))
	params.Set("s", salt)
	params.Set("v", "1.16.1")
	params.Set("c", "dropwatch")
	params.Set("f", "json")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/rest/"+endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subsonic: %s returned %s", endpoint, resp.Status)
	}
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("subsonic: %s: %w", endpoint, err)
	}
	if out.Resp.Status != "ok" {
		if out.Resp.Error != nil {
			return nil, fmt.Errorf("subsonic: %s: %s (code %d)", endpoint, out.Resp.Error.Message, out.Resp.Error.Code)
		}
		return nil, fmt.Errorf("subsonic: %s: status %q", endpoint, out.Resp.Status)
	}
	return &out, nil
}

// Ping verifies connectivity and credentials.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, "ping", url.Values{})
	return err
}

// Library returns all album artists with their albums. This is one request
// per artist, so large libraries take a while against remote servers.
func (c *Client) Library(ctx context.Context, progress func(done, total int)) ([]Artist, error) {
	res, err := c.call(ctx, "getArtists", url.Values{})
	if err != nil {
		return nil, err
	}
	if res.Resp.Artists == nil {
		return nil, fmt.Errorf("subsonic: getArtists returned no artist list")
	}
	type ref struct{ id, name string }
	var refs []ref
	for _, idx := range res.Resp.Artists.Index {
		for _, a := range idx.Artist {
			refs = append(refs, ref{a.ID, a.Name})
		}
	}
	out := make([]Artist, 0, len(refs))
	for i, r := range refs {
		res, err := c.call(ctx, "getArtist", url.Values{"id": {r.id}})
		if err != nil {
			return nil, fmt.Errorf("getArtist %q: %w", r.name, err)
		}
		artist := Artist{Name: r.name}
		if res.Resp.Artist != nil {
			for _, alb := range res.Resp.Artist.Album {
				artist.Albums = append(artist.Albums, Album{Title: alb.Name, MBID: alb.MusicBrainzID})
			}
		}
		out = append(out, artist)
		if progress != nil {
			progress(i+1, len(refs))
		}
	}
	return out, nil
}

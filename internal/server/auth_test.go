package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dropwatch/internal/mpdclient"
)

type client struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
}

func (c *client) do(method, path, body string, header ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookie {
			c.cookie = ck
		}
	}
	return rec
}

func TestAuthFlow(t *testing.T) {
	st := syncStore(t)
	s := New(st, nil, "test")
	if err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, handler: s.Handler()}
	state := func() map[string]bool {
		var m map[string]bool
		json.Unmarshal(c.do("GET", "/api/auth", "").Body.Bytes(), &m)
		return m
	}
	if m := state(); !m["setupRequired"] || m["authenticated"] {
		t.Fatalf("fresh state = %v", m)
	}
	if rec := c.do("GET", "/api/artists", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("API open before setup: %d", rec.Code)
	}
	if rec := c.do("GET", "/", ""); rec.Code != http.StatusOK {
		t.Fatalf("page not served: %d", rec.Code)
	}
	if rec := c.do("POST", "/api/auth/setup", `{"password":"short"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password accepted: %d", rec.Code)
	}
	if rec := c.do("POST", "/api/auth/setup", `{"password":"correct horse"}`); rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	if m := state(); m["setupRequired"] || !m["authenticated"] {
		t.Fatalf("after setup = %v", m)
	}
	other := &client{t: t, handler: c.handler}
	if rec := other.do("POST", "/api/auth/setup", `{"password":"takeover attempt"}`); rec.Code != http.StatusConflict {
		t.Fatalf("second setup allowed: %d", rec.Code)
	}
	if rec := c.do("GET", "/api/artists", ""); rec.Code != http.StatusOK {
		t.Fatalf("session rejected: %d", rec.Code)
	}
	// Cookie-authenticated writes need the CSRF header.
	if rec := c.do("PUT", "/api/settings", `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("write without CSRF header: %d", rec.Code)
	}
	settings := `{"mpd":{"enabled":true,"address":"localhost","password":"mpdsecret"},"subsonic":{"enabled":false,"url":"","user":""},"melody":{"enabled":false,"address":""},"discogs":{"token":"dctoken"}}`
	rec := c.do("PUT", "/api/settings", settings, csrfHeader, "1")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "mpdsecret") || strings.Contains(rec.Body.String(), "dctoken") {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body)
	}
	// A form without the password field keeps the stored one.
	keep := `{"mpd":{"enabled":true,"address":"otherhost"},"subsonic":{"enabled":false,"url":"","user":""},"melody":{"enabled":false,"address":""},"discogs":{}}`
	if rec := c.do("PUT", "/api/settings", keep, csrfHeader, "1"); rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body)
	}
	if cfg := s.config(); cfg.MPD.Password != "mpdsecret" || cfg.MPD.Address != "otherhost" || cfg.Discogs.Token != "dctoken" || s.dc() == nil {
		t.Fatalf("secrets not kept: %+v", cfg)
	}
	var tok map[string]string
	json.Unmarshal(c.do("POST", "/api/auth/token", "", csrfHeader, "1").Body.Bytes(), &tok)
	if rec := other.do("GET", "/api/artists", "", "Authorization", "Bearer "+tok["token"]); rec.Code != http.StatusOK {
		t.Fatalf("API token rejected: %d", rec.Code)
	}
	if rec := other.do("GET", "/api/artists", "", "Authorization", "Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad API token accepted: %d", rec.Code)
	}
	if rec := c.do("POST", "/api/auth/logout", ""); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := c.do("GET", "/api/artists", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session survived logout: %d", rec.Code)
	}
	if rec := c.do("POST", "/api/auth/login", `{"password":"correct horse"}`); rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	if rec := c.do("PUT", "/api/auth/password", `{"current":"wrong","new":"another password"}`, csrfHeader, "1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("password changed without the current one: %d", rec.Code)
	}
}

func TestMergeLibraries(t *testing.T) {
	merged := mergeLibraries(
		[]mpdclientArtist{{Name: "The Ocean", Albums: albums("Pelagial", "rg-1")}},
		[]mpdclientArtist{{Name: "Ocean", Albums: albums("Pelagial", "rel-1", "Pelagial", "rg-1")}, {Name: "Low"}},
	)
	if len(merged) != 2 || merged[0].Name != "The Ocean" || len(merged[0].Albums) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
}

type mpdclientArtist = mpdclient.Artist

func albums(pairs ...string) []mpdclient.Album {
	var out []mpdclient.Album
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, mpdclient.Album{Title: pairs[i], MBID: pairs[i+1]})
	}
	return out
}

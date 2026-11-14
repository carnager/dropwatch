package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestNotificationsCORS(t *testing.T) {
	st := syncStore(t)
	s := New(st, nil, "test")
	if err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, handler: s.Handler()}
	if rec := c.do("POST", "/api/auth/setup", `{"password":"correct horse"}`); rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	var tok map[string]string
	json.Unmarshal(c.do("POST", "/api/auth/token", "", csrfHeader, "1").Body.Bytes(), &tok)
	page := &client{t: t, handler: s.Handler()} // a start page: no session cookie

	pre := page.do("OPTIONS", "/api/notifications", "", "Origin", "null", "Access-Control-Request-Headers", "authorization")
	if pre.Code != http.StatusNoContent || pre.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", pre.Code, pre.Header())
	}
	if rec := page.do("GET", "/api/notifications", "", "Origin", "null"); rec.Code != http.StatusUnauthorized ||
		rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("no token: %d %v", rec.Code, rec.Header())
	}
	rec := page.do("GET", "/api/notifications", "", "Origin", "null", "Authorization", "Bearer "+tok["token"])
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("with token: %d %s", rec.Code, rec.Body)
	}
	if rec := page.do("POST", "/api/notifications/read", "", "Authorization", "Bearer "+tok["token"]); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS leaked onto a write")
	}
	if rec := page.do("GET", "/api/artists", "", "Origin", "null", "Authorization", "Bearer "+tok["token"]); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS on an endpoint that is not listed")
	}
}

func TestFeedTokenIsReadOnly(t *testing.T) {
	st := syncStore(t)
	s := New(st, nil, "test")
	if err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, handler: s.Handler()}
	if rec := c.do("POST", "/api/auth/setup", `{"password":"correct horse"}`); rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	var api, feed map[string]string
	json.Unmarshal(c.do("POST", "/api/auth/token", "", csrfHeader, "1").Body.Bytes(), &api)
	json.Unmarshal(c.do("POST", "/api/auth/feed-token", "", csrfHeader, "1").Body.Bytes(), &feed)
	if feed["token"] == "" || feed["token"] == api["token"] {
		t.Fatalf("feed token: %v", feed)
	}
	page := &client{t: t, handler: s.Handler()}
	bearer := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

	if rec := page.do("GET", "/api/notifications", "", bearer(feed["token"])...); rec.Code != http.StatusOK {
		t.Fatalf("feed token on notifications: %d %s", rec.Code, rec.Body)
	}
	for _, req := range [][2]string{{"GET", "/api/artists"}, {"GET", "/api/settings"}, {"POST", "/api/notifications/read"}, {"POST", "/api/auth/token"}} {
		if rec := page.do(req[0], req[1], "", bearer(feed["token"])...); rec.Code != http.StatusUnauthorized {
			t.Fatalf("feed token reached %s %s: %d", req[0], req[1], rec.Code)
		}
	}
	// The two are independent: creating one leaves the other working.
	if rec := page.do("GET", "/api/artists", "", bearer(api["token"])...); rec.Code != http.StatusOK {
		t.Fatalf("API token after feed token: %d", rec.Code)
	}
	var settings map[string]any
	json.Unmarshal(c.do("GET", "/api/settings", "").Body.Bytes(), &settings)
	if settings["feedTokenSet"] != true || settings["apiTokenSet"] != true {
		t.Fatalf("settings flags: %v", settings)
	}
	if rec := c.do("DELETE", "/api/auth/feed-token", "", csrfHeader, "1"); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := page.do("GET", "/api/notifications", "", bearer(feed["token"])...); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked feed token still works: %d", rec.Code)
	}
}

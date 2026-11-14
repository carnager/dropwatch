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

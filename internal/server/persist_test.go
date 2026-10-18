package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"dropwatch/internal/model"
	"dropwatch/internal/store"
	"dropwatch/internal/watch"
)

// TestSettingsSurviveRestart saves every setting through the API the way the
// settings tabs do (each tab sends only its own section), reopens the
// database as a restart would, and checks nothing was lost.
func TestSettingsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	dumps := t.TempDir()
	open := func() (*store.Store, *Server, *watch.Watcher, *client) {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s := New(st, nil, "test")
		if err := s.LoadConfig(); err != nil {
			t.Fatal(err)
		}
		w, err := watch.New(st, dumps, "test")
		if err != nil {
			t.Fatal(err)
		}
		s.SetWatcher(w)
		return st, s, w, &client{t: t, handler: s.Handler()}
	}
	st, s, w, c := open()
	put := func(path, body string) {
		t.Helper()
		if rec := c.do("PUT", path, body, csrfHeader, "1"); rec.Code != 200 {
			t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := c.do("POST", "/api/auth/setup", `{"password":"correct horse"}`); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	var tok map[string]string
	json.Unmarshal(c.do("POST", "/api/auth/token", "", csrfHeader, "1").Body.Bytes(), &tok)

	// Library tab, then push tab, then the schedule: each sends its own part.
	put("/api/settings", `{"mpd":{"enabled":false,"address":"mpd.local","password":"mpdpw"},
		"subsonic":{"enabled":true,"url":"https://nav.example","user":"rasi","password":"subpw"},
		"melody":{"enabled":true,"address":"melody.local:6603","password":"melpw"},
		"discogs":{"token":"dctoken"},"sync":{"intervalHours":6}}`)
	put("/api/settings", `{"ntfy":{"enabled":true,"server":"https://ntfy.example","topic":"releases","token":"ntfytk",
		"user":"","types":["Album","EP"],"linkUrl":"https://dw.example"}}`)
	// Saving the library tab again without touching passwords keeps them.
	put("/api/settings", `{"mpd":{"enabled":false,"address":"mpd.local"},
		"subsonic":{"enabled":true,"url":"https://nav.example","user":"rasi"},
		"melody":{"enabled":true,"address":"melody.local:6603"},"discogs":{},"sync":{"intervalHours":6}}`)
	put("/api/dumps/schedule", `{"enabled":true,"days":[0,4],"time":"04:30","timezone":"Europe/Berlin"}`)
	put("/api/dumps/releases", `{"enabled":true,"intervalDays":14}`)
	w.Close()
	st.Close()

	st, s, w, _ = open()
	defer st.Close()
	defer w.Close()
	cfg := s.config()
	want := model.Config{
		MPD:      model.MPDConfig{Address: "mpd.local", Password: "mpdpw"},
		Subsonic: model.SubsonicConfig{Enabled: true, URL: "https://nav.example", User: "rasi", Password: "subpw"},
		Melody:   model.MelodyConfig{Enabled: true, Address: "melody.local:6603", Password: "melpw"},
		Discogs:  model.DiscogsConfig{Token: "dctoken"},
		Sync:     model.SyncConfig{IntervalHours: 6},
	}
	if cfg.MPD != want.MPD || cfg.Subsonic != want.Subsonic || cfg.Melody != want.Melody || cfg.Discogs != want.Discogs || cfg.Sync != want.Sync {
		t.Fatalf("sources after restart:\n got %+v\nwant %+v", cfg, want)
	}
	n := cfg.Ntfy
	if !n.Enabled || n.Server != "https://ntfy.example" || n.Topic != "releases" || n.Token != "ntfytk" ||
		len(n.Types) != 2 || n.LinkURL != "https://dw.example" || n.Since.IsZero() {
		t.Fatalf("ntfy after restart: %+v", n)
	}
	view := w.View()
	if !view.Schedule.Enabled || view.Schedule.Time != "04:30" || view.Schedule.Timezone != "Europe/Berlin" || len(view.Schedule.Days) != 2 ||
		!view.Releases.Enabled || view.Releases.IntervalDays != 14 {
		t.Fatalf("dump settings after restart: %+v", view)
	}
	if ok, err := st.CheckAdminPassword("correct horse"); err != nil || !ok {
		t.Fatalf("admin password lost: %v %v", ok, err)
	}
	if ok, err := st.ValidAPIToken(tok["token"]); err != nil || !ok {
		t.Fatalf("API token lost: %v %v", ok, err)
	}
}

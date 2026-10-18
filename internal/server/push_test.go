package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dropwatch/internal/model"
)

type fakeNtfy struct {
	mu       sync.Mutex
	messages []map[string]any
	auth     []string
	fail     bool
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"limit reached"}`))
		return
	}
	var m map[string]any
	json.NewDecoder(r.Body).Decode(&m)
	f.messages = append(f.messages, m)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
}

func (f *fakeNtfy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func TestPushPending(t *testing.T) {
	st := syncStore(t)
	trackArtist(t, st, "artist", "Insomnium")
	fake := &fakeNtfy{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s := New(st, nil, "test")
	enabled := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.applyConfig(model.Config{Ntfy: model.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "releases", Token: "tk",
		Types: []string{"Album", "EP"}, LinkURL: "https://dw.example", Since: enabled}})

	apply := func(now time.Time, groups ...model.ReleaseGroup) {
		t.Helper()
		if err := st.ApplyDump(context.Background(), map[string]bool{"artist": true},
			map[string][]model.ReleaseGroup{"artist": groups}, now, time.UTC); err != nil {
			t.Fatal(err)
		}
	}
	group := func(id, typ, date string) model.ReleaseGroup {
		return model.ReleaseGroup{ID: id, MBID: id, ArtistID: "artist", Title: id, NormTitle: id, PrimaryType: typ, FirstReleaseDate: date}
	}
	// Baseline before ntfy was enabled: its notification must not be pushed.
	apply(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), group("Backlog", "Album", "2026-09-15"))
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	apply(now, group("Backlog", "Album", "2026-09-15"), group("Netherworlds", "Album", "2026-10-04"), group("A Single", "Single", "2026-10-04"))
	if err := s.pushPending(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 1 {
		t.Fatalf("pushed %d messages: %v", fake.count(), fake.messages)
	}
	m := fake.messages[0]
	if m["topic"] != "releases" || m["title"] != "Insomnium" || !strings.HasPrefix(m["message"].(string), "Netherworlds\nNew album") ||
		m["click"] != "https://dw.example/#artist/artist" || fake.auth[0] != "Bearer tk" {
		t.Fatalf("message = %v, auth = %v", m, fake.auth)
	}
	if err := s.pushPending(context.Background(), now); err != nil || fake.count() != 1 {
		t.Fatalf("pushed twice: %d %v", fake.count(), err)
	}

	// A failed push is retried; a big batch becomes one summary.
	var many []model.ReleaseGroup
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		many = append(many, group(id, "EP", "2026-10-05"))
	}
	later := now.Add(time.Hour)
	apply(later, many...)
	fake.fail = true
	if err := s.pushPending(context.Background(), later); err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("expected ntfy error, got %v", err)
	}
	fake.fail = false
	if err := s.pushPending(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 2 || fake.messages[1]["title"] != "7 new releases" || fake.messages[1]["click"] != "https://dw.example/#notifications" {
		t.Fatalf("summary = %v", fake.messages)
	}
}

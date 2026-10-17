package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dropwatch/internal/model"
	"dropwatch/internal/store"
	"dropwatch/internal/watch"
)

func TestDumpSettingsAndCachedArtist(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	watcher, err := watch.New(st, t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	s := New(st, nil, "test") // A live API call would fail this test.
	s.SetWatcher(watcher)
	s.maxAge = -time.Hour
	handler := s.Handler()
	token, err := st.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(response, req)
		return response
	}
	for _, invalid := range []string{
		`{"enabled":true,"days":[],"time":"04:00","timezone":"UTC"}`,
		`{"enabled":true,"days":[0],"time":"04:00","timezone":"invalid"}`,
		`{"enabled":true,"days":[0],"time":"04:00","timezone":"UTC","typo":true}`,
		`{"enabled":true,"days":[0],"time":"04:00","timezone":"UTC"} {}`,
	} {
		if response := request(http.MethodPut, "/api/dumps/schedule", invalid); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid settings accepted: %d %s", response.Code, response.Body)
		}
	}
	valid := `{"enabled":true,"days":[0,4],"time":"04:00","timezone":"Europe/Berlin"}`
	if response := request(http.MethodPut, "/api/dumps/schedule", valid); response.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", response.Code, response.Body)
	}
	response := request(http.MethodGet, "/api/dumps", "")
	var view watch.View
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || !view.Schedule.Enabled || view.Schedule.Timezone != "Europe/Berlin" || !view.NextRun.After(time.Now()) {
		t.Fatalf("schedule round trip: %s %v", response.Body, err)
	}
	if err := st.UpsertArtist(model.Artist{ID: "artist", Name: "Artist"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyDump(context.Background(), map[string]bool{"artist": true}, map[string][]model.ReleaseGroup{
		"artist": {{ID: "album", MBID: "album", Title: "Album", PrimaryType: "Album"}},
	}, time.Now(), time.UTC); err != nil {
		t.Fatal(err)
	}
	response = request(http.MethodGet, "/api/artists/artist/releases", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"title":"Album"`) {
		t.Fatalf("dump cache not served: %d %s", response.Code, response.Body)
	}
	response = request(http.MethodGet, "/api/notifications", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"notifications":[]`) {
		t.Fatalf("notification baseline: %d %s", response.Code, response.Body)
	}
}

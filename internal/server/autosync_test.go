package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"dropwatch/internal/model"
)

// fakeSubsonic serves one artist with one album.
func fakeSubsonic(t *testing.T, reads *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := `{"subsonic-response":{"status":"ok"%s}}`
		switch r.URL.Path {
		case "/rest/ping":
			fmt.Fprintf(w, ok, "")
		case "/rest/getArtists":
			reads.Add(1)
			fmt.Fprintf(w, ok, `,"artists":{"index":[{"artist":[{"id":"1","name":"Insomnium"}]}]}`)
		case "/rest/getArtist":
			fmt.Fprintf(w, ok, `,"artist":{"album":[{"name":"Winter's Gate"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitForSync(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 200; i++ {
		s.jobMu.Lock()
		job := s.job
		s.jobMu.Unlock()
		if job != nil {
			job.mu.Lock()
			running := job.Running
			job.mu.Unlock()
			if !running {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sync did not finish")
}

func TestAutoSync(t *testing.T) {
	st := syncStore(t)
	trackArtist(t, st, "insomnium", "Insomnium", "Winter's Gate", "Anno 1696")
	var reads atomic.Int32
	sub := fakeSubsonic(t, &reads)
	s := New(st, nil, "test") // known artist: no MusicBrainz requests
	cfg := model.Config{Subsonic: model.SubsonicConfig{Enabled: true, URL: sub.URL, User: "u", Password: "p"}}
	s.applyConfig(cfg)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	s.autoSyncTick(context.Background(), now)
	if reads.Load() != 0 {
		t.Fatal("synced with automatic sync off")
	}
	cfg.Sync.IntervalHours = 6
	s.applyConfig(cfg)
	s.autoSyncTick(context.Background(), now)
	waitForSync(t, s)
	if reads.Load() != 1 {
		t.Fatalf("library read %d times, want 1", reads.Load())
	}
	if got := owned(t, st, "insomnium"); len(got) != 1 || got[0] != "Winter's Gate" {
		t.Fatalf("owned = %v", got)
	}
	s.autoSyncTick(context.Background(), now.Add(5*time.Hour))
	if reads.Load() != 1 {
		t.Fatal("synced again before the interval passed")
	}
	s.autoSyncTick(context.Background(), now.Add(6*time.Hour))
	waitForSync(t, s)
	if reads.Load() != 2 {
		t.Fatalf("library read %d times after the interval, want 2", reads.Load())
	}
}

package watch

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dropwatch/internal/model"
	"dropwatch/internal/store"
)

func fixtureArchive(t *testing.T, data string) []byte {
	t.Helper()
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz required for archive integration test")
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: "mbdump/release-group", Mode: 0600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("xz", "-c")
	cmd.Stdin = &raw
	compressed, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return compressed
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertArtist(model.Artist{ID: "artist", Name: "Artist"}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDownloadCheckAndDiscovery(t *testing.T) {
	st := testStore(t)
	baseline := `{"id":"old","title":"Old","primary-type":"Album","first-release-date":"1990","artist-credit":[{"artist":{"id":"artist"}}]}`
	recent := fmt.Sprintf(`{"id":"new","title":"New","primary-type":"Album","first-release-date":%q,"artist-credit":[{"artist":{"id":"artist"}}]}`, time.Now().UTC().Format("2006-01-02"))
	var mu sync.Mutex
	version, archive, badChecksum := "20260902-001001", fixtureArchive(t, baseline), false
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("User-Agent") != "dropwatch-test" {
			t.Error("missing User-Agent")
		}
		switch r.URL.Path {
		case "/LATEST":
			fmt.Fprintln(w, version)
		case "/" + version + "/SHA256SUMS":
			checksum := fmt.Sprintf("%x", sha256.Sum256(archive))
			if badChecksum {
				checksum = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  release-group.tar.xz\n", checksum)
		case "/" + version + "/release-group.tar.xz":
			downloads.Add(1)
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	w := &Watcher{store: st, dir: dir, userAgent: "dropwatch-test", baseURL: server.URL + "/", client: server.Client()}
	previous, err := w.check(context.Background(), "", time.UTC)
	if err != nil || previous != version {
		t.Fatalf("baseline check: %s %v", previous, err)
	}
	if _, err := w.check(context.Background(), previous, time.UTC); err != nil || downloads.Load() != 1 {
		t.Fatalf("re-downloaded same version: %d %v", downloads.Load(), err)
	}
	mu.Lock()
	version, archive = "20260905-001001", fixtureArchive(t, baseline+"\n"+recent)
	mu.Unlock()
	previous, err = w.check(context.Background(), previous, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	ns, unread, err := st.Notifications(time.Now())
	if err != nil || len(ns) != 1 || unread != 1 || ns[0].Title != "New" {
		t.Fatalf("discovery: %v %d %v", ns, unread, err)
	}
	mu.Lock()
	version, badChecksum = "20260909-001001", true
	mu.Unlock()
	if got, err := w.check(context.Background(), previous, time.UTC); err == nil || got != previous {
		t.Fatalf("bad checksum accepted: %s %v", got, err)
	}
	mu.Lock()
	badChecksum, archive = false, fixtureArchive(t, baseline+"\n{")
	mu.Unlock()
	if got, err := w.check(context.Background(), previous, time.UTC); err == nil || got != previous {
		t.Fatalf("partial dump accepted: %s %v", got, err)
	}
	groups, err := st.ReleaseGroups("artist")
	if err != nil || len(groups) != 2 {
		t.Fatalf("failed scan changed stored data: %v %v", groups, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary downloads not removed: %v %v", entries, err)
	}
}

func TestScheduleRestartAndOverlappingChecks(t *testing.T) {
	st := testStore(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	schedule := model.DumpSchedule{Enabled: true, Days: []int{0, 4}, Time: "04:00", Timezone: "Europe/Berlin"}
	settings := Settings{Schedule: schedule, NextRun: time.Now().Add(-24 * time.Hour)}
	if err := st.SaveDumpSetting("schedule", settings); err != nil {
		t.Fatal(err)
	}
	w, err := newWatcher(st, t.TempDir(), "test", server.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("missed schedule was not caught up on startup")
	}
	if err := w.CheckNow(); err != ErrRunning {
		t.Fatalf("overlapping check: %v", err)
	}
	view := w.View()
	if !view.NextRun.After(time.Now()) || view.Schedule.Timezone != "Europe/Berlin" {
		t.Fatalf("schedule not preserved/advanced: %+v", view)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for w.View().Status.Running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if view := w.View(); view.Status.Running || view.Status.LastError == "" || view.Status.Version != "" {
		t.Fatalf("failure not persisted: %+v", view)
	}
	w.Close()
	w, err = newWatcher(st, t.TempDir(), "test", server.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if view := w.View(); view.Status.Running || view.Status.LastError == "" || !view.NextRun.After(time.Now()) {
		t.Fatalf("restart lost status: %+v", view)
	}
	schedule.Enabled = false
	if err := w.Configure(schedule); err != nil {
		t.Fatal(err)
	}
	if !w.View().NextRun.IsZero() {
		t.Fatal("disabled schedule still has next run")
	}
}

package watch

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dropwatch/internal/model"
)

func TestReleaseDumpAppliesTitlesForKnownGroups(t *testing.T) {
	st := testStore(t)
	if err := st.SaveReleaseGroups("artist", []model.ReleaseGroup{{ID: "rg-aaliyah", MBID: "rg-aaliyah", ArtistID: "artist",
		Title: "Aaliyah", NormTitle: model.NormalizeTitle("Aaliyah"), PrimaryType: "Album"}}); err != nil {
		t.Fatal(err)
	}
	records := strings.Join([]string{
		`{"id":"rel-1","title":"Aaliyah: Edition 2004","release-group":{"id":"rg-aaliyah"},"media":[]}`,
		`{"id":"rel-2","title":"Somebody Else","release-group":{"id":"rg-unknown"}}`,
	}, "\n") + "\n"
	var mu sync.Mutex
	archive, bad := fixtureArchive(t, records), false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/LATEST":
			fmt.Fprintln(w, "20261014-001001")
		case "/20261014-001001/SHA256SUMS":
			sum := fmt.Sprintf("%x", sha256.Sum256(archive))
			if bad {
				sum = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s *release.tar.xz\n%s *release-group.tar.xz\n", sum, strings.Repeat("1", 64))
		case "/20261014-001001/release.tar.xz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	w := &Watcher{store: st, dir: t.TempDir(), userAgent: "test", baseURL: server.URL + "/", client: server.Client()}

	mu.Lock()
	bad = true
	mu.Unlock()
	if _, err := w.checkReleases(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	if aliases, _ := st.Aliases("artist"); len(aliases) != 0 {
		t.Fatalf("unverified titles saved: %v", aliases)
	}
	mu.Lock()
	bad = false
	mu.Unlock()
	version, err := w.checkReleases(context.Background(), "")
	if err != nil || version != "20261014-001001" {
		t.Fatalf("release dump: %s %v", version, err)
	}
	aliases, err := st.Aliases("artist")
	if err != nil || len(aliases) != 2 || aliases[0].ReleaseMBID != "rel-1" {
		t.Fatalf("aliases = %+v, err = %v", aliases, err)
	}
	if again, err := w.checkReleases(context.Background(), version); err != nil || again != version {
		t.Fatalf("same version reprocessed: %s %v", again, err)
	}
}

func TestNextReleaseRun(t *testing.T) {
	loc := time.UTC
	enabled := time.Date(2026, 10, 1, 12, 0, 0, 0, loc) // Thursday
	w := &Watcher{settings: Settings{Schedule: model.DumpSchedule{Enabled: true, Days: []int{0, 4}, Time: "04:00", Timezone: "UTC"}},
		releases: ReleaseSettings{Enabled: true, IntervalDays: 30, EnabledAt: enabled}}
	if got := w.nextReleaseRunLocked(); !got.Equal(time.Date(2026, 10, 4, 4, 0, 0, 0, loc)) {
		t.Fatalf("first run = %v", got)
	}
	w.relStatus.LastAttempt = time.Date(2026, 10, 4, 4, 0, 5, 0, loc)
	w.relStatus.LastSuccess = time.Date(2026, 10, 4, 9, 0, 0, 0, loc)
	if got := w.nextReleaseRunLocked(); got.Before(time.Date(2026, 11, 2, 0, 0, 0, 0, loc)) || got.After(time.Date(2026, 11, 6, 0, 0, 0, 0, loc)) {
		t.Fatalf("after success = %v", got)
	}
	w.relStatus.LastAttempt = time.Date(2026, 11, 5, 4, 0, 5, 0, loc) // failed attempt
	if got := w.nextReleaseRunLocked(); !got.Equal(time.Date(2026, 11, 8, 4, 0, 0, 0, loc)) {
		t.Fatalf("retry after failure = %v", got)
	}
	w.settings.Schedule.Enabled = false
	if got := w.nextReleaseRunLocked(); !got.IsZero() {
		t.Fatalf("runs with schedule off: %v", got)
	}
}

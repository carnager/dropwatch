// Package watch schedules dump downloads and discovers releases without polling
// the MusicBrainz web service.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"dropwatch/internal/dump"
	"dropwatch/internal/model"
	"dropwatch/internal/store"
)

const dumpURL = "https://data.metabrainz.org/pub/musicbrainz/data/json-dumps/"

var ErrRunning = errors.New("a dump check is already running")

type Status struct {
	Running     bool      `json:"running"`
	Phase       string    `json:"phase"`
	Bytes       int64     `json:"bytes"`
	TotalBytes  int64     `json:"totalBytes"`
	Records     int       `json:"records"`
	LastAttempt time.Time `json:"lastAttempt"`
	LastSuccess time.Time `json:"lastSuccess"`
	Version     string    `json:"version"`
	LastError   string    `json:"lastError"`
}

type Settings struct {
	Schedule model.DumpSchedule `json:"schedule"`
	NextRun  time.Time          `json:"nextRun"`
}

type View struct {
	Settings
	Status Status `json:"status"`
}

type Watcher struct {
	store     *store.Store
	dir       string
	userAgent string
	baseURL   string
	client    *http.Client
	mu        sync.Mutex
	settings  Settings
	status    Status
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
}

func New(st *store.Store, dir, userAgent string) (*Watcher, error) {
	return newWatcher(st, dir, userAgent, dumpURL)
}

func newWatcher(st *store.Store, dir, userAgent, baseURL string) (*Watcher, error) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{store: st, dir: dir, userAgent: userAgent, baseURL: baseURL,
		client: &http.Client{Timeout: 8 * time.Hour}, ctx: ctx, cancel: cancel,
		settings: Settings{Schedule: model.DefaultDumpSchedule()}}
	if err := st.LoadDumpSetting("schedule", &w.settings); err != nil {
		cancel()
		return nil, err
	}
	if err := w.settings.Schedule.Validate(); err != nil {
		cancel()
		return nil, err
	}
	if err := st.LoadDumpSetting("status", &w.status); err != nil {
		cancel()
		return nil, err
	}
	if w.status.Running {
		w.status.LastError = "Previous check was interrupted; it will retry at the next scheduled time."
	}
	w.status.Running, w.status.Phase = false, "idle"
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

func (w *Watcher) Close() {
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait()
}

func (w *Watcher) View() View {
	w.mu.Lock()
	defer w.mu.Unlock()
	settings := w.settings
	settings.Schedule.Days = append([]int{}, settings.Schedule.Days...)
	return View{Settings: settings, Status: w.status}
}

func (w *Watcher) Configure(schedule model.DumpSchedule) error {
	if err := schedule.Validate(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	settings := Settings{Schedule: schedule, NextRun: schedule.Next(time.Now())}
	if err := w.store.SaveDumpSetting("schedule", settings); err != nil {
		return err
	}
	w.settings = settings
	return nil
}

func (w *Watcher) loop() {
	defer w.wg.Done()
	w.tick(time.Now()) // One catch-up check if the server missed a saved run.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			w.tick(now)
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *Watcher) tick(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.status.Running || !w.settings.Schedule.Enabled || w.settings.NextRun.IsZero() || now.Before(w.settings.NextRun) {
		return
	}
	next := w.settings
	next.NextRun = next.Schedule.Next(now)
	if err := w.store.SaveDumpSetting("schedule", next); err != nil {
		log.Printf("dump schedule: %v", err)
		return
	}
	w.settings = next
	if err := w.startLocked(); err != nil {
		log.Printf("starting dump check: %v", err)
	}
}

func (w *Watcher) CheckNow() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.startLocked()
}

func (w *Watcher) startLocked() error {
	if w.closed {
		return fmt.Errorf("dump watcher is shutting down")
	}
	if w.status.Running {
		return ErrRunning
	}
	status := w.status
	status.Running, status.Phase, status.LastError = true, "checking", ""
	status.LastAttempt = time.Now()
	status.Bytes, status.TotalBytes, status.Records = 0, 0, 0
	if err := w.store.SaveDumpSetting("status", status); err != nil {
		return err
	}
	w.status = status
	loc, _ := time.LoadLocation(w.settings.Schedule.Timezone)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		version, err := w.check(w.ctx, status.Version, loc)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.status.Running = false
		if err != nil {
			w.status.Phase, w.status.LastError = "failed", err.Error()
			log.Printf("dump check failed: %v", err)
		} else {
			w.status.Phase, w.status.LastSuccess = "complete", time.Now()
			w.status.Version = version
			log.Printf("dump check complete (version %s)", version)
		}
		if err := w.store.SaveDumpSetting("status", w.status); err != nil {
			log.Printf("saving dump status: %v", err)
		}
	}()
	return nil
}

var versionPattern = regexp.MustCompile(`^\d{8}-\d{6}$`)

func (w *Watcher) check(ctx context.Context, previous string, loc *time.Location) (string, error) {
	artists, err := w.store.Artists()
	if err != nil {
		return previous, err
	}
	if len(artists) == 0 {
		return previous, fmt.Errorf("track or import artists before checking for new releases")
	}
	tracked := make(map[string]bool, len(artists))
	needsBaseline := false
	for _, artist := range artists {
		tracked[artist.ID] = true
		baseline, err := w.store.HasDumpBaseline(artist.ID)
		if err != nil {
			return previous, err
		}
		needsBaseline = needsBaseline || !baseline
	}
	latest, err := w.smallFile(ctx, w.baseURL+"LATEST")
	if err != nil {
		return previous, err
	}
	version := strings.TrimSpace(string(latest))
	if !versionPattern.MatchString(version) {
		return previous, fmt.Errorf("invalid dump version in LATEST: %q", version)
	}
	if version == previous && !needsBaseline {
		return previous, nil
	}
	manifest, err := w.smallFile(ctx, w.baseURL+version+"/SHA256SUMS")
	if err != nil {
		return previous, err
	}
	checksum := ""
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == "release-group.tar.xz" {
			checksum = strings.ToLower(fields[0])
		}
	}
	decoded, err := hex.DecodeString(checksum)
	if err != nil || len(decoded) != sha256.Size {
		return previous, fmt.Errorf("missing or invalid release-group checksum")
	}
	if err := os.MkdirAll(w.dir, 0755); err != nil {
		return previous, err
	}
	file, err := os.CreateTemp(w.dir, "release-group-*.tar.xz")
	if err != nil {
		return previous, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	resp, err := w.get(ctx, w.baseURL+version+"/release-group.tar.xz")
	if err != nil {
		return previous, err
	}
	defer resp.Body.Close()
	w.mu.Lock()
	w.status.Phase, w.status.TotalBytes = "downloading", resp.ContentLength
	w.mu.Unlock()
	hash := sha256.New()
	counter := &progressWriter{watcher: w}
	if _, err := io.Copy(io.MultiWriter(file, hash, counter), resp.Body); err != nil {
		return previous, fmt.Errorf("downloading dump: %w", err)
	}
	if err := file.Close(); err != nil {
		return previous, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return previous, fmt.Errorf("dump checksum mismatch; download discarded")
	}
	w.mu.Lock()
	w.status.Phase = "scanning"
	w.mu.Unlock()
	groups, err := dump.ReadGroups(ctx, file.Name(), tracked, func(records int) {
		w.mu.Lock()
		w.status.Records = records
		w.mu.Unlock()
	})
	if err != nil {
		return previous, err
	}
	w.mu.Lock()
	w.status.Phase = "saving"
	w.mu.Unlock()
	if err := w.store.ApplyDump(ctx, tracked, groups, time.Now(), loc); err != nil {
		return previous, err
	}
	return version, nil
}

type progressWriter struct{ watcher *Watcher }

func (p *progressWriter) Write(data []byte) (int, error) {
	p.watcher.mu.Lock()
	p.watcher.status.Bytes += int64(len(data))
	p.watcher.mu.Unlock()
	return len(data), nil
}

func (w *Watcher) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", w.userAgent)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("dump server returned %s", resp.Status)
	}
	return resp, nil
}

func (w *Watcher) smallFile(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	resp, err := w.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	const limit = 64 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && len(data) > limit {
		err = fmt.Errorf("dump metadata exceeds %d bytes", limit)
	}
	return data, err
}

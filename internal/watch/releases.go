package watch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"dropwatch/internal/model"
	"dropwatch/internal/store"
)

// The release dump (~21 GB) lists every version of every album. Its titles
// ("Aaliyah: Edition 2004") and release MBIDs (what Subsonic and melody tag
// albums with) let library albums match their release group exactly. It is
// never stored: the download streams through xz and the parser, and only
// titles of release groups already in the database are kept.

// ReleaseSettings controls the optional release-dump refresh. It runs on the
// release-group schedule's days and time, at most every IntervalDays.
type ReleaseSettings struct {
	Enabled      bool      `json:"enabled"`
	IntervalDays int       `json:"intervalDays"`
	EnabledAt    time.Time `json:"enabledAt"` // anchors the first run
}

func (r ReleaseSettings) Validate() error {
	if r.IntervalDays < 1 || r.IntervalDays > 365 {
		return fmt.Errorf("interval must be between 1 and 365 days")
	}
	return nil
}

type ReleaseView struct {
	ReleaseSettings
	NextRun time.Time `json:"nextRun"`
	Status  Status    `json:"status"`
}

// nextReleaseRunLocked is the first scheduled slot at least IntervalDays
// after the last success, and after the last attempt (a failure retries at the
// next slot), counted from when the refresh was switched on.
func (w *Watcher) nextReleaseRunLocked() time.Time {
	if !w.releases.Enabled || !w.settings.Schedule.Enabled {
		return time.Time{}
	}
	from := w.releases.EnabledAt
	if !w.relStatus.LastSuccess.IsZero() {
		// A day early, so the scheduled slot on the due day itself counts.
		if due := w.relStatus.LastSuccess.AddDate(0, 0, w.releases.IntervalDays-1); due.After(from) {
			from = due
		}
	}
	if w.relStatus.LastAttempt.After(from) {
		from = w.relStatus.LastAttempt
	}
	return w.settings.Schedule.Next(from)
}

func (w *Watcher) ConfigureReleases(r ReleaseSettings) error {
	if err := r.Validate(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	r.EnabledAt = w.releases.EnabledAt
	if r.Enabled && (!w.releases.Enabled || r.EnabledAt.IsZero()) {
		r.EnabledAt = time.Now()
	}
	if err := w.store.SaveDumpSetting("release_settings", r); err != nil {
		return err
	}
	w.releases = r
	return nil
}

func (w *Watcher) CheckReleasesNow() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.startReleasesLocked()
}

func (w *Watcher) startReleasesLocked() error {
	if w.closed {
		return fmt.Errorf("dump watcher is shutting down")
	}
	if w.relStatus.Running {
		return ErrRunning
	}
	status := w.relStatus
	status.Running, status.Phase, status.LastError = true, "checking", ""
	status.LastAttempt = time.Now()
	status.Bytes, status.TotalBytes, status.Records = 0, 0, 0
	if err := w.store.SaveDumpSetting("release_status", status); err != nil {
		return err
	}
	w.relStatus = status
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		version, err := w.checkReleases(w.ctx, status.Version)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.relStatus.Running = false
		if err != nil {
			w.relStatus.Phase, w.relStatus.LastError = "failed", err.Error()
			log.Printf("release dump failed: %v", err)
		} else {
			w.relStatus.Phase, w.relStatus.LastSuccess = "complete", time.Now()
			w.relStatus.Version = version
			log.Printf("release dump complete (version %s)", version)
		}
		if err := w.store.SaveDumpSetting("release_status", w.relStatus); err != nil {
			log.Printf("saving release dump status: %v", err)
		}
	}()
	return nil
}

func (w *Watcher) setReleasePhase(phase string, total int64) {
	w.mu.Lock()
	w.relStatus.Phase = phase
	if total != 0 {
		w.relStatus.TotalBytes = total
	}
	w.mu.Unlock()
}

func (w *Watcher) checkReleases(ctx context.Context, previous string) (string, error) {
	known, err := w.store.KnownReleaseGroups()
	if err != nil {
		return previous, err
	}
	if len(known) == 0 {
		return previous, fmt.Errorf("sync your library first: release titles are only kept for known albums")
	}
	latest, err := w.smallFile(ctx, w.baseURL+"LATEST")
	if err != nil {
		return previous, err
	}
	version := strings.TrimSpace(string(latest))
	if !versionPattern.MatchString(version) {
		return previous, fmt.Errorf("invalid dump version in LATEST: %q", version)
	}
	if version == previous {
		return previous, nil
	}
	checksum, err := w.checksum(ctx, version, "release.tar.xz")
	if err != nil {
		return previous, err
	}
	resp, err := w.get(ctx, w.baseURL+version+"/release.tar.xz")
	if err != nil {
		return previous, err
	}
	defer resp.Body.Close()
	w.setReleasePhase("downloading", resp.ContentLength)

	// Hash every byte as it arrives; tar may stop reading at the end-of-archive
	// marker, so whatever follows is drained into the hash afterwards.
	hash := sha256.New()
	body := io.TeeReader(resp.Body, io.MultiWriter(hash, &releaseProgress{w}))
	cmd := exec.CommandContext(ctx, "tar", "-xJOf", "-", "--wildcards", "mbdump/*")
	cmd.Stdin = body
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return previous, err
	}
	if err := cmd.Start(); err != nil {
		return previous, fmt.Errorf("starting tar (tar and xz are required): %w", err)
	}
	titles, scanErr := w.readReleaseTitles(ctx, stdout, known)
	if scanErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return previous, scanErr
	}
	if waitErr != nil {
		return previous, fmt.Errorf("extracting release dump: %w", waitErr)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return previous, fmt.Errorf("downloading release dump: %w", err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return previous, fmt.Errorf("release dump checksum mismatch; nothing was saved")
	}
	w.setReleasePhase("saving", 0)
	if err := w.store.ApplyReleaseTitles(ctx, titles); err != nil {
		return previous, err
	}
	return version, nil
}

type releaseProgress struct{ w *Watcher }

func (p *releaseProgress) Write(data []byte) (int, error) {
	p.w.mu.Lock()
	p.w.relStatus.Bytes += int64(len(data))
	p.w.mu.Unlock()
	return len(data), nil
}

// readReleaseTitles keeps the titles and MBIDs of releases in known groups.
// A malformed record fails the run: with a checksummed download it means the
// stream broke, and a partial result must not replace good data.
func (w *Watcher) readReleaseTitles(ctx context.Context, input io.Reader, known map[string][]string) (map[string][]store.Alias, error) {
	reader := bufio.NewReaderSize(input, 1<<20)
	titles := map[string][]store.Alias{}
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var r struct {
				ID           string `json:"id"`
				Title        string `json:"title"`
				ReleaseGroup struct {
					ID string `json:"id"`
				} `json:"release-group"`
			}
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("release record %d: %w", count+1, err)
			}
			count++
			if count%100000 == 0 {
				w.mu.Lock()
				w.relStatus.Phase, w.relStatus.Records = "scanning", count
				w.mu.Unlock()
			}
			if _, ok := known[r.ReleaseGroup.ID]; ok && r.ID != "" {
				norm, light := model.NormalizeTitle(r.Title), model.NormalizeTitleLight(r.Title)
				titles[r.ReleaseGroup.ID] = append(titles[r.ReleaseGroup.ID], store.Alias{RGID: r.ReleaseGroup.ID, NormTitle: norm, ReleaseMBID: r.ID})
				if light != norm {
					titles[r.ReleaseGroup.ID] = append(titles[r.ReleaseGroup.ID], store.Alias{RGID: r.ReleaseGroup.ID, NormTitle: light, ReleaseMBID: r.ID})
				}
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("release dump is empty")
	}
	w.mu.Lock()
	w.relStatus.Records = count
	w.mu.Unlock()
	return titles, nil
}

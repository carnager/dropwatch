package server

import (
	"context"
	"errors"
	"log"
	"time"
)

// autoSyncState is the last automatic sync attempt, kept across restarts so a
// restart neither repeats a sync nor skips a missed one.
type autoSyncState struct {
	LastRun   time.Time `json:"lastRun"`
	LastError string    `json:"lastError"`
}

// RunAutoSync starts an "only new" library sync whenever the configured
// interval has passed since the last attempt, checking once a minute.
func (s *Server) RunAutoSync(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		s.autoSyncTick(ctx, time.Now())
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) autoSyncTick(ctx context.Context, now time.Time) {
	hours := s.config().Sync.IntervalHours
	if hours <= 0 || len(enabledSources(s.config())) == 0 {
		return
	}
	var state autoSyncState
	if err := s.store.LoadDumpSetting("auto_sync", &state); err != nil {
		log.Printf("auto sync: %v", err)
		return
	}
	if now.Sub(state.LastRun) < time.Duration(hours)*time.Hour {
		return
	}
	err := s.startLibrarySync(ctx, "", false, true)
	if errors.Is(err, errSyncRunning) {
		return // try again next minute
	}
	state = autoSyncState{LastRun: now}
	if err != nil {
		state.LastError = err.Error()
		log.Printf("auto sync: %v", err)
	} else {
		log.Printf("auto sync started")
	}
	if err := s.store.SaveDumpSetting("auto_sync", state); err != nil {
		log.Printf("auto sync: %v", err)
	}
}

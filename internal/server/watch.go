package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dropwatch/internal/model"
	"dropwatch/internal/watch"
)

func (s *Server) handleDumpStatus(w http.ResponseWriter, r *http.Request) {
	if s.watcher == nil {
		writeErr(w, http.StatusServiceUnavailable, "dump scheduling is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.watcher.View())
}

func (s *Server) handleDumpSchedule(w http.ResponseWriter, r *http.Request) {
	if s.watcher == nil {
		writeErr(w, http.StatusServiceUnavailable, "dump scheduling is unavailable")
		return
	}
	var schedule model.DumpSchedule
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&schedule); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid schedule: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "expected one schedule object")
		return
	}
	if err := schedule.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.watcher.Configure(schedule); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.watcher.View())
}

func (s *Server) handleDumpCheck(w http.ResponseWriter, r *http.Request) {
	if s.watcher == nil {
		writeErr(w, http.StatusServiceUnavailable, "dump scheduling is unavailable")
		return
	}
	if err := s.watcher.CheckNow(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, watch.ErrRunning) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.watcher.View())
}

// notificationTypes reads ?types=Album,EP,Single; absent means all types.
func notificationTypes(r *http.Request) ([]string, error) {
	raw := r.URL.Query().Get("types")
	if raw == "" {
		return nil, nil
	}
	var types []string
	for _, t := range strings.Split(raw, ",") {
		switch t {
		case "Album", "EP", "Single":
			types = append(types, t)
		default:
			return nil, errors.New("types must be a comma-separated list of Album, EP and Single")
		}
	}
	return types, nil
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	types, err := notificationTypes(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	notifications, unread, err := s.store.Notifications(time.Now(), types)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": notifications, "unread": unread})
}

func (s *Server) handleReadNotifications(w http.ResponseWriter, r *http.Request) {
	types, err := notificationTypes(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.ReadNotifications(time.Now(), types); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleReleaseSettings(w http.ResponseWriter, r *http.Request) {
	if s.watcher == nil {
		writeErr(w, http.StatusServiceUnavailable, "dump scheduling is unavailable")
		return
	}
	var settings watch.ReleaseSettings
	if !decodeOne(w, r, 4096, &settings) {
		return
	}
	if err := settings.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.watcher.ConfigureReleases(settings); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.watcher.View())
}

func (s *Server) handleReleaseCheck(w http.ResponseWriter, r *http.Request) {
	if s.watcher == nil {
		writeErr(w, http.StatusServiceUnavailable, "dump scheduling is unavailable")
		return
	}
	if err := s.watcher.CheckReleasesNow(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, watch.ErrRunning) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.watcher.View())
}

func (s *Server) handleReadNotification(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid notification id")
		return
	}
	found, err := s.store.ReadNotification(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no such notification")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

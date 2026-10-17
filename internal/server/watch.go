package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	notifications, unread, err := s.store.Notifications(time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": notifications, "unread": unread})
}

func (s *Server) handleReadNotifications(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ReadNotifications(time.Now()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

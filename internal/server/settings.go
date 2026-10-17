package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"dropwatch/internal/model"
)

// decodeOne reads exactly one JSON object of at most limit bytes.
func decodeOne(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "expected one JSON object")
		return false
	}
	return true
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	apiToken, err := s.store.HasAPIToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": s.config().Public(), "apiTokenSet": apiToken})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var update model.ConfigUpdate
	if !decodeOne(w, r, 16<<10, &update) {
		return
	}
	cfg := update.Apply(s.config())
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SaveConfig(cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.applyConfig(cfg)
	s.handleGetSettings(w, r)
}

// handleTestSource connects to one source using the settings form as it is
// now (unsaved changes included, stored secrets filling the gaps).
func (s *Server) handleTestSource(w http.ResponseWriter, r *http.Request) {
	var update model.ConfigUpdate
	if !decodeOne(w, r, 16<<10, &update) {
		return
	}
	src, ok := sourceFor(update.Apply(s.config()), r.PathValue("name"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown source")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := src.ping(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

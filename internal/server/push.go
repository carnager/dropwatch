package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"dropwatch/internal/model"
	"dropwatch/internal/ntfy"
)

// Up to this many due notifications are pushed one by one; more become a
// single summary, so a big dump check doesn't buzz a phone dozens of times.
const maxSinglePushes = 5

func ntfyClient(c model.NtfyConfig) *ntfy.Client {
	return ntfy.New(c.ServerURL(), c.Topic, c.Token, c.User, c.Password)
}

// RunPusher pushes due notifications to ntfy every minute until ctx ends.
func (s *Server) RunPusher(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := s.pushPending(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("ntfy: %v", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) pushPending(ctx context.Context, now time.Time) error {
	cfg := s.config().Ntfy
	if !cfg.Enabled {
		return nil
	}
	pending, err := s.store.PendingPushes(now, cfg.Since)
	if err != nil || len(pending) == 0 {
		return err
	}
	var send []model.ReleaseNotification
	var skipped []int64
	for _, n := range pending {
		if cfg.Wants(n.PrimaryType) {
			send = append(send, n)
		} else {
			skipped = append(skipped, n.ID)
		}
	}
	if err := s.store.MarkPushed(skipped, now); err != nil {
		return err
	}
	client := ntfyClient(cfg)
	if len(send) > maxSinglePushes {
		if err := client.Publish(ctx, summaryMessage(send, cfg.LinkURL)); err != nil {
			return err // nothing marked: retried next minute
		}
		return s.store.MarkPushed(ids(send), now)
	}
	for _, n := range send {
		if err := client.Publish(ctx, releaseMessage(n, cfg.LinkURL)); err != nil {
			return err
		}
		if err := s.store.MarkPushed([]int64{n.ID}, now); err != nil {
			return err
		}
	}
	return nil
}

func ids(ns []model.ReleaseNotification) []int64 {
	out := make([]int64, len(ns))
	for i, n := range ns {
		out[i] = n.ID
	}
	return out
}

func typeWord(primaryType string) string {
	switch primaryType {
	case "EP":
		return "EP"
	case "":
		return "release"
	default:
		return strings.ToLower(primaryType)
	}
}

func releaseLine(n model.ReleaseNotification) string {
	switch n.Kind {
	case "released":
		return fmt.Sprintf("Out today: %s", typeWord(n.PrimaryType))
	case "upcoming":
		return fmt.Sprintf("Upcoming %s, out %s", typeWord(n.PrimaryType), n.ReleaseDate)
	default:
		return fmt.Sprintf("New %s, %s", typeWord(n.PrimaryType), n.ReleaseDate)
	}
}

func releaseMessage(n model.ReleaseNotification, link string) ntfy.Message {
	m := ntfy.Message{Title: n.ArtistName, Message: n.Title + "\n" + releaseLine(n), Tags: []string{"cd"}}
	if link != "" {
		m.Click = link + "/#artist/" + n.ArtistID
	}
	return m
}

func summaryMessage(ns []model.ReleaseNotification, link string) ntfy.Message {
	const listed = 15
	var b strings.Builder
	for i, n := range ns {
		if i == listed {
			fmt.Fprintf(&b, "and %d more", len(ns)-listed)
			break
		}
		fmt.Fprintf(&b, "%s: %s (%s)\n", n.ArtistName, n.Title, typeWord(n.PrimaryType))
	}
	m := ntfy.Message{Title: fmt.Sprintf("%d new releases", len(ns)), Message: strings.TrimSpace(b.String()), Tags: []string{"cd"}}
	if link != "" {
		m.Click = link + "/#notifications"
	}
	return m
}

// handleNtfyTest sends a test message with the settings form as it is now.
func (s *Server) handleNtfyTest(w http.ResponseWriter, r *http.Request) {
	var update model.ConfigUpdate
	if !decodeOne(w, r, 16<<10, &update) {
		return
	}
	cfg := update.Apply(s.config()).Ntfy
	if cfg.Topic == "" {
		writeErr(w, http.StatusBadRequest, "enter a topic first")
		return
	}
	m := ntfy.Message{Title: "dropwatch", Message: "Test message. New releases will arrive like this.", Tags: []string{"cd"}}
	if cfg.LinkURL != "" {
		m.Click = cfg.LinkURL + "/#notifications"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := ntfyClient(cfg).Publish(ctx, m); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

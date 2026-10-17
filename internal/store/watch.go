package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"dropwatch/internal/model"
)

const watchSchema = `
CREATE INDEX IF NOT EXISTS idx_rg_id ON release_groups(id);
CREATE TABLE IF NOT EXISTS dump_settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS dump_baselines (
	artist_id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS dump_seen (
	artist_id TEXT NOT NULL,
	rg_id TEXT NOT NULL,
	PRIMARY KEY (artist_id, rg_id)
);
CREATE TABLE IF NOT EXISTS release_notifications (
	id INTEGER PRIMARY KEY,
	artist_id TEXT NOT NULL,
	rg_id TEXT NOT NULL,
	title TEXT NOT NULL,
	release_date TEXT NOT NULL,
	kind TEXT NOT NULL,
	available_at TEXT NOT NULL,
	unread INTEGER NOT NULL DEFAULT 1,
	UNIQUE (rg_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_notification_due ON release_notifications(available_at);
`

func (s *Store) LoadDumpSetting(key string, value any) error {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM dump_settings WHERE key = ?`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), value)
}

func (s *Store) SaveDumpSetting(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO dump_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(raw))
	return err
}

func (s *Store) HasDumpBaseline(artistID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dump_baselines WHERE artist_id = ?)`, artistID).Scan(&exists)
	return exists, err
}

// ApplyDump updates the tracked subset and its notification baseline in one
// transaction. Dump refreshes are additive: ownership, aliases, Discogs data and
// groups absent from a snapshot survive. A failed/cancelled scan never calls this.
func (s *Store) ApplyDump(ctx context.Context, tracked map[string]bool, groups map[string][]model.ReleaseGroup, now time.Time, loc *time.Location) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upsert, err := tx.PrepareContext(ctx, `INSERT INTO release_groups
		(id, artist_id, title, norm_title, primary_type, secondary_types, first_release_date, mbid, sources, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'musicbrainz',
			COALESCE((SELECT state FROM release_groups WHERE id = ? AND state != '' LIMIT 1), ''))
		ON CONFLICT(artist_id, id) DO UPDATE SET
			title = excluded.title, norm_title = excluded.norm_title,
			primary_type = excluded.primary_type, secondary_types = excluded.secondary_types,
			first_release_date = excluded.first_release_date, mbid = excluded.mbid,
			sources = CASE WHEN instr(',' || release_groups.sources || ',', ',musicbrainz,') > 0 THEN release_groups.sources
				WHEN release_groups.sources = '' THEN 'musicbrainz' ELSE release_groups.sources || ',musicbrainz' END`)
	if err != nil {
		return err
	}
	defer upsert.Close()
	markSeen, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO dump_seen (artist_id, rg_id) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer markSeen.Close()
	notify, err := tx.PrepareContext(ctx, `INSERT INTO release_notifications
		(artist_id, rg_id, title, release_date, kind, available_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(rg_id, kind) DO UPDATE SET title = excluded.title, release_date = excluded.release_date,
			available_at = excluded.available_at WHERE release_notifications.available_at > ?`)
	if err != nil {
		return err
	}
	defer notify.Close()
	stamp := now.UTC().Format(time.RFC3339)
	for artistID := range tracked {
		var exists, baseline bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artists WHERE id = ?),
			EXISTS(SELECT 1 FROM dump_baselines WHERE artist_id = ?)`, artistID, artistID).Scan(&exists, &baseline); err != nil {
			return err
		}
		if !exists { // The artist may have been untracked during the download.
			continue
		}
		for _, g := range groups[artistID] {
			if _, err := upsert.ExecContext(ctx, g.ID, artistID, g.Title, g.NormTitle, g.PrimaryType,
				strings.Join(g.SecondaryTypes, ","), g.FirstReleaseDate, g.MBID, g.ID); err != nil {
				return err
			}
			result, err := markSeen.ExecContext(ctx, artistID, g.ID)
			if err != nil {
				return err
			}
			added, err := result.RowsAffected()
			if err != nil {
				return err
			}
			// Date-only releases become due at midnight in the selected timezone.
			// Partial dates can announce upcoming music but never invent a day.
			date, dateEnd, precise := releaseDateRange(g.FirstReleaseDate, loc)
			eligible := len(g.SecondaryTypes) == 0 && (g.PrimaryType == "Album" || g.PrimaryType == "EP" || g.PrimaryType == "Single")
			recent := !date.IsZero() && !dateEnd.Before(now.AddDate(0, 0, -30))
			if precise && eligible && recent {
				// A release may be moved earlier as well as postponed. Only adjust
				// reminders that have not become visible yet.
				if _, err := tx.ExecContext(ctx, `UPDATE release_notifications SET title = ?, release_date = ?, available_at = ?
					WHERE rg_id = ? AND kind = 'released' AND available_at > ?`,
					g.Title, g.FirstReleaseDate, date.UTC().Format(time.RFC3339), g.ID, stamp); err != nil {
					return err
				}
			} else {
				if _, err := tx.ExecContext(ctx, `DELETE FROM release_notifications WHERE rg_id = ? AND kind = 'released' AND available_at > ?`, g.ID, stamp); err != nil {
					return err
				}
			}
			if !eligible || !recent {
				continue // Historic additions and undated records aren't new releases.
			}
			if baseline && added > 0 {
				kind := "new"
				if date.After(now) {
					kind = "upcoming"
				}
				if _, err := notify.ExecContext(ctx, artistID, g.ID, g.Title, g.FirstReleaseDate, kind, stamp, stamp); err != nil {
					return err
				}
			}
			if precise && date.After(now) {
				if _, err := notify.ExecContext(ctx, artistID, g.ID, g.Title, g.FirstReleaseDate, "released", date.UTC().Format(time.RFC3339), stamp); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dump_baselines (artist_id, created_at) VALUES (?, ?)`, artistID, stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func releaseDateRange(value string, loc *time.Location) (time.Time, time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if len(value) != len(layout) {
			continue
		}
		start, err := time.ParseInLocation(layout, value, loc)
		if err != nil {
			break
		}
		switch layout {
		case "2006-01-02":
			return start, start.AddDate(0, 0, 1), true
		case "2006-01":
			return start, start.AddDate(0, 1, 0), false
		default:
			return start, start.AddDate(1, 0, 0), false
		}
	}
	return time.Time{}, time.Time{}, false
}

func (s *Store) Notifications(now time.Time) ([]model.ReleaseNotification, int, error) {
	stamp := now.UTC().Format(time.RFC3339)
	var unread int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM release_notifications n JOIN artists a ON a.id = n.artist_id
		WHERE n.available_at <= ? AND n.unread = 1`, stamp).Scan(&unread); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT n.id, n.artist_id, a.name, n.rg_id, n.title, n.release_date, n.kind, n.unread
		FROM release_notifications n JOIN artists a ON a.id = n.artist_id
		WHERE n.available_at <= ? ORDER BY n.available_at DESC, n.id DESC LIMIT 200`, stamp)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.ReleaseNotification{}
	for rows.Next() {
		var n model.ReleaseNotification
		if err := rows.Scan(&n.ID, &n.ArtistID, &n.ArtistName, &n.ReleaseID, &n.Title, &n.ReleaseDate, &n.Kind, &n.Unread); err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	return out, unread, rows.Err()
}

func (s *Store) ReadNotifications(now time.Time) error {
	_, err := s.db.Exec(`UPDATE release_notifications SET unread = 0 WHERE available_at <= ?`, now.UTC().Format(time.RFC3339))
	return err
}

// Package store persists artists, release groups and ownership state in SQLite.
package store

import (
	"database/sql"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"dropwatch/internal/model"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc sqlite is not safe for concurrent writes on multiple conns.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate upgrades old databases where release_groups was keyed by id alone.
// Release groups are shared between credited artists (collaboration albums),
// so the key must be (artist_id, id); the old key silently kept such albums
// under whichever artist fetched them first.
func migrate(db *sql.DB) error {
	var pkCols int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('release_groups') WHERE pk > 0`).Scan(&pkCols)
	if err != nil || pkCols != 1 {
		return err // already composite (or empty table just created with new schema)
	}
	_, err = db.Exec(`
		BEGIN;
		DROP TABLE IF EXISTS release_groups_v2;
		CREATE TABLE release_groups_v2 (
			id                 TEXT NOT NULL,
			artist_id          TEXT NOT NULL REFERENCES artists(id),
			title              TEXT NOT NULL,
			norm_title         TEXT NOT NULL,
			primary_type       TEXT NOT NULL DEFAULT '',
			secondary_types    TEXT NOT NULL DEFAULT '',
			first_release_date TEXT NOT NULL DEFAULT '',
			mbid               TEXT NOT NULL DEFAULT '',
			discogs_master_id  INTEGER NOT NULL DEFAULT 0,
			sources            TEXT NOT NULL DEFAULT '',
			state              TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (artist_id, id)
		);
		INSERT INTO release_groups_v2
			SELECT id, artist_id, title, norm_title, primary_type, secondary_types,
			       first_release_date, mbid, discogs_master_id, sources, state
			FROM release_groups;
		DROP TABLE release_groups;
		ALTER TABLE release_groups_v2 RENAME TO release_groups;
		CREATE INDEX IF NOT EXISTS idx_rg_artist ON release_groups(artist_id);
		COMMIT;`)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS artists (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	sort_name      TEXT NOT NULL DEFAULT '',
	disambiguation TEXT NOT NULL DEFAULT '',
	country        TEXT NOT NULL DEFAULT '',
	discogs_id     INTEGER NOT NULL DEFAULT 0,
	last_checked   TIMESTAMP
);
CREATE TABLE IF NOT EXISTS release_groups (
	id                 TEXT NOT NULL,
	artist_id          TEXT NOT NULL REFERENCES artists(id),
	title              TEXT NOT NULL,
	norm_title         TEXT NOT NULL,
	primary_type       TEXT NOT NULL DEFAULT '',
	secondary_types    TEXT NOT NULL DEFAULT '',
	first_release_date TEXT NOT NULL DEFAULT '',
	mbid               TEXT NOT NULL DEFAULT '',
	discogs_master_id  INTEGER NOT NULL DEFAULT 0,
	sources            TEXT NOT NULL DEFAULT '',
	state              TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (artist_id, id)
);
CREATE INDEX IF NOT EXISTS idx_rg_artist ON release_groups(artist_id);
CREATE TABLE IF NOT EXISTS release_aliases (
	artist_id    TEXT NOT NULL,
	rg_id        TEXT NOT NULL,
	norm_title   TEXT NOT NULL,
	release_mbid TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (artist_id, rg_id, norm_title, release_mbid)
);
CREATE INDEX IF NOT EXISTS idx_alias_artist ON release_aliases(artist_id);
`

func (s *Store) Close() error { return s.db.Close() }

// UpsertArtist saves an artist, preserving an existing discogs_id if the new
// value is zero.
func (s *Store) UpsertArtist(a model.Artist) error {
	_, err := s.db.Exec(`
		INSERT INTO artists (id, name, sort_name, disambiguation, country, discogs_id, last_checked)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			sort_name = excluded.sort_name,
			disambiguation = excluded.disambiguation,
			country = excluded.country,
			discogs_id = CASE WHEN excluded.discogs_id > 0 THEN excluded.discogs_id ELSE artists.discogs_id END,
			last_checked = excluded.last_checked`,
		a.ID, a.Name, a.SortName, a.Disambiguation, a.Country, a.DiscogsID, time.Now())
	return err
}

// Artists returns all tracked artists with owned/missing counts over their
// canonical albums/EPs, most-missing first.
func (s *Store) Artists() ([]model.ArtistSummary, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.name, a.sort_name, a.disambiguation, a.country, a.discogs_id,
			COUNT(rg.id),
			COALESCE(SUM(CASE WHEN rg.state = 'owned' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rg.state = '' THEN 1 ELSE 0 END), 0)
		FROM artists a
		LEFT JOIN release_groups rg ON rg.artist_id = a.id
			AND ((LOWER(rg.primary_type) IN ('album', 'ep') AND rg.secondary_types = '')
			     OR rg.state = 'owned')
		GROUP BY a.id
		ORDER BY COALESCE(SUM(CASE WHEN rg.state = '' THEN 1 ELSE 0 END), 0) DESC, a.sort_name, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ArtistSummary
	for rows.Next() {
		var a model.ArtistSummary
		if err := rows.Scan(&a.ID, &a.Name, &a.SortName, &a.Disambiguation, &a.Country, &a.DiscogsID, &a.Total, &a.Owned, &a.Missing); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ArtistByName finds a tracked artist by exact name, case-insensitively.
func (s *Store) ArtistByName(name string) (model.Artist, bool, error) {
	var a model.Artist
	err := s.db.QueryRow(`SELECT id, name, sort_name, disambiguation, country, discogs_id FROM artists WHERE name = ? COLLATE NOCASE`, name).
		Scan(&a.ID, &a.Name, &a.SortName, &a.Disambiguation, &a.Country, &a.DiscogsID)
	if err == sql.ErrNoRows {
		return a, false, nil
	}
	return a, err == nil, err
}

func (s *Store) Artist(id string) (model.Artist, bool, error) {
	var a model.Artist
	err := s.db.QueryRow(`SELECT id, name, sort_name, disambiguation, country, discogs_id FROM artists WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &a.SortName, &a.Disambiguation, &a.Country, &a.DiscogsID)
	if err == sql.ErrNoRows {
		return a, false, nil
	}
	return a, err == nil, err
}

// SaveReleaseGroups upserts freshly fetched groups, preserving user state.
// If a Discogs-only entry later gains an MBID-keyed twin (or vice versa),
// state carries over via norm_title.
func (s *Store) SaveReleaseGroups(artistID string, groups []model.ReleaseGroup) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	states := map[string]model.State{}
	rows, err := tx.Query(`SELECT norm_title, state FROM release_groups WHERE artist_id = ? AND state != ''`, artistID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var norm, state string
		if err := rows.Scan(&norm, &state); err != nil {
			rows.Close()
			return err
		}
		states[norm] = model.State(state)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// A release group shared with another tracked artist (collaboration
	// albums) inherits that copy's state, so an album owned under one artist
	// is owned under all of them.
	statesByID := map[string]model.State{}
	rows, err = tx.Query(`SELECT DISTINCT id, state FROM release_groups WHERE state != ''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return err
		}
		statesByID[id] = model.State(state)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range groups {
		state := g.State
		if state == model.StateNone {
			state = states[g.NormTitle]
		}
		if state == model.StateNone {
			state = statesByID[g.ID]
		}
		_, err := tx.Exec(`
			INSERT INTO release_groups (id, artist_id, title, norm_title, primary_type, secondary_types, first_release_date, mbid, discogs_master_id, sources, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(artist_id, id) DO UPDATE SET
				title = excluded.title,
				norm_title = excluded.norm_title,
				primary_type = excluded.primary_type,
				secondary_types = excluded.secondary_types,
				first_release_date = excluded.first_release_date,
				mbid = excluded.mbid,
				discogs_master_id = excluded.discogs_master_id,
				sources = excluded.sources,
				state = CASE WHEN release_groups.state != '' THEN release_groups.state ELSE excluded.state END`,
			g.ID, artistID, g.Title, g.NormTitle, g.PrimaryType,
			strings.Join(g.SecondaryTypes, ","), g.FirstReleaseDate,
			g.MBID, g.DiscogsMasterID, strings.Join(g.Sources, ","), string(state))
		if err != nil {
			return err
		}
	}
	// Prune rows that no longer come back from the sources (e.g. bootlegs we
	// now drop, or merged-away duplicates) — but never rows the user marked.
	ids := make([]any, 0, len(groups)+1)
	ids = append(ids, artistID)
	placeholders := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
		placeholders = append(placeholders, "?")
	}
	q := `DELETE FROM release_groups WHERE artist_id = ? AND state = ''`
	if len(placeholders) > 0 {
		q += ` AND id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if _, err := tx.Exec(q, ids...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReleaseGroups(artistID string) ([]model.ReleaseGroup, error) {
	rows, err := s.db.Query(`
		SELECT id, artist_id, title, norm_title, primary_type, secondary_types, first_release_date, mbid, discogs_master_id, sources, state
		FROM release_groups WHERE artist_id = ?
		ORDER BY first_release_date DESC, title`, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ReleaseGroup
	for rows.Next() {
		var g model.ReleaseGroup
		var secondary, srcs, state string
		if err := rows.Scan(&g.ID, &g.ArtistID, &g.Title, &g.NormTitle, &g.PrimaryType, &secondary, &g.FirstReleaseDate, &g.MBID, &g.DiscogsMasterID, &srcs, &state); err != nil {
			return nil, err
		}
		if secondary != "" {
			g.SecondaryTypes = strings.Split(secondary, ",")
		}
		if srcs != "" {
			g.Sources = strings.Split(srcs, ",")
		}
		g.State = model.State(state)
		g.Missing = g.State == model.StateNone
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetState marks a release group owned/ignored/none.
func (s *Store) SetState(releaseGroupID string, state model.State) (bool, error) {
	res, err := s.db.Exec(`UPDATE release_groups SET state = ? WHERE id = ?`, string(state), releaseGroupID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Alias is a stored release-title alias pointing at a release group.
type Alias struct {
	RGID        string
	NormTitle   string
	ReleaseMBID string
}

// SaveAliases replaces the artist's release-title aliases.
func (s *Store) SaveAliases(artistID string, aliases []Alias) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM release_aliases WHERE artist_id = ?`, artistID); err != nil {
		return err
	}
	for _, a := range aliases {
		_, err := tx.Exec(`INSERT OR IGNORE INTO release_aliases (artist_id, rg_id, norm_title, release_mbid) VALUES (?, ?, ?, ?)`,
			artistID, a.RGID, a.NormTitle, a.ReleaseMBID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Aliases returns the artist's release-title aliases.
func (s *Store) Aliases(artistID string) ([]Alias, error) {
	rows, err := s.db.Query(`SELECT rg_id, norm_title, release_mbid FROM release_aliases WHERE artist_id = ?`, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alias
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.RGID, &a.NormTitle, &a.ReleaseMBID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Library returns all owned release groups with their artists, one row per
// (artist, release group) — shared collab albums appear under each artist.
func (s *Store) Library() ([]model.LibraryEntry, error) {
	rows, err := s.db.Query(`
		SELECT a.name, rg.id, rg.artist_id, rg.title, rg.primary_type,
		       rg.secondary_types, rg.first_release_date, rg.sources
		FROM release_groups rg
		JOIN artists a ON a.id = rg.artist_id
		WHERE rg.state = 'owned'
		ORDER BY a.sort_name, a.name, rg.first_release_date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LibraryEntry
	for rows.Next() {
		var e model.LibraryEntry
		var secondary, srcs string
		if err := rows.Scan(&e.ArtistName, &e.ID, &e.ArtistID, &e.Title, &e.PrimaryType,
			&secondary, &e.FirstReleaseDate, &srcs); err != nil {
			return nil, err
		}
		if secondary != "" {
			e.SecondaryTypes = strings.Split(secondary, ",")
		}
		if srcs != "" {
			e.Sources = strings.Split(srcs, ",")
		}
		e.State = model.StateOwned
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteArtist removes an artist and all its release groups (including
// ownership state).
func (s *Store) DeleteArtist(id string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM release_groups WHERE artist_id = ?`, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM release_aliases WHERE artist_id = ?`, id); err != nil {
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM artists WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, tx.Commit()
}

// LastChecked returns when the artist's releases were last fetched.
func (s *Store) LastChecked(artistID string) (time.Time, error) {
	var t sql.NullTime
	err := s.db.QueryRow(`SELECT last_checked FROM artists WHERE id = ?`, artistID).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return t.Time, nil
}

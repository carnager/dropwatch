package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"dropwatch/internal/model"
)

const settingsSchema = `
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	last_seen TEXT NOT NULL
);
`

// SessionLifetime is how long a login stays valid without use.
const SessionLifetime = 30 * 24 * time.Hour

func (s *Store) loadSetting(key string, value any) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), value)
}

func (s *Store) saveSetting(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(raw))
	return err
}

// Config returns the stored configuration and whether one was ever saved.
func (s *Store) Config() (model.Config, bool, error) {
	var c model.Config
	found, err := s.loadSetting("config", &c)
	return c, found, err
}

func (s *Store) SaveConfig(c model.Config) error { return s.saveSetting("config", c) }

// passwordHash is PBKDF2-SHA256 with a per-password salt.
type passwordHash struct {
	Salt       string `json:"salt"`
	Iterations int    `json:"iterations"`
	Hash       string `json:"hash"`
}

const pbkdf2Iterations = 600000

func hashPassword(password string, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iterations, 32)
}

// HasAdminPassword reports whether first-run setup is done.
func (s *Store) HasAdminPassword() (bool, error) {
	var h passwordHash
	return s.loadSetting("admin_password", &h)
}

// SetAdminPassword replaces the admin password and ends every session.
func (s *Store) SetAdminPassword(password string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key, err := hashPassword(password, salt, pbkdf2Iterations)
	if err != nil {
		return err
	}
	if err := s.saveSetting("admin_password", passwordHash{Salt: hex.EncodeToString(salt), Iterations: pbkdf2Iterations, Hash: hex.EncodeToString(key)}); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM sessions`)
	return err
}

// CheckAdminPassword reports whether password is the admin password.
func (s *Store) CheckAdminPassword(password string) (bool, error) {
	var h passwordHash
	found, err := s.loadSetting("admin_password", &h)
	if err != nil || !found {
		return false, err
	}
	salt, err := hex.DecodeString(h.Salt)
	if err != nil {
		return false, err
	}
	want, err := hex.DecodeString(h.Hash)
	if err != nil {
		return false, err
	}
	got, err := hashPassword(password, salt, h.Iterations)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func newToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(raw)
	return token, tokenHash(token), nil
}

// Tokens are high-entropy, so a plain hash is enough to keep a database copy
// from handing out working credentials.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession returns a new session token for a successful login.
func (s *Store) CreateSession(now time.Time) (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	stamp := now.UTC().Format(time.RFC3339)
	_, err = s.db.Exec(`INSERT INTO sessions (token_hash, created_at, last_seen) VALUES (?, ?, ?)`, hash, stamp, stamp)
	return token, err
}

// ValidSession reports whether token is a live session and refreshes it.
func (s *Store) ValidSession(token string, now time.Time) (bool, error) {
	if token == "" {
		return false, nil
	}
	cutoff := now.Add(-SessionLifetime).UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`UPDATE sessions SET last_seen = ? WHERE token_hash = ? AND last_seen > ?`,
		now.UTC().Format(time.RFC3339), tokenHash(token), cutoff)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash(token))
	return err
}

// NewAPIToken replaces the API token (for scripts and players) and returns
// it; only its hash is stored, so it can be shown exactly once.
func (s *Store) NewAPIToken() (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	return token, s.saveSetting("api_token", hash)
}

func (s *Store) RevokeAPIToken() error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = 'api_token'`)
	return err
}

func (s *Store) HasAPIToken() (bool, error) {
	var hash string
	return s.loadSetting("api_token", &hash)
}

func (s *Store) ValidAPIToken(token string) (bool, error) {
	var hash string
	found, err := s.loadSetting("api_token", &hash)
	if err != nil || !found || token == "" {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(tokenHash(token)), []byte(hash)) == 1, nil
}

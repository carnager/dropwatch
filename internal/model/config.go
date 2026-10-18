package model

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Config is everything set up in the web UI. Secrets (passwords, tokens) are
// stored here but never sent back to the browser; see Public.
type Config struct {
	MPD      MPDConfig      `json:"mpd"`
	Subsonic SubsonicConfig `json:"subsonic"`
	Melody   MelodyConfig   `json:"melody"`
	Discogs  DiscogsConfig  `json:"discogs"`
	Ntfy     NtfyConfig     `json:"ntfy"`
	Sync     SyncConfig     `json:"sync"`
}

// SyncConfig runs an "only new" library sync on an interval.
type SyncConfig struct {
	IntervalHours int `json:"intervalHours"` // 0: off
}

// NtfyConfig sends release notifications to an ntfy topic.
type NtfyConfig struct {
	Enabled  bool     `json:"enabled"`
	Server   string   `json:"server"` // default https://ntfy.sh
	Topic    string   `json:"topic"`
	Token    string   `json:"token,omitempty"` // access token, or:
	User     string   `json:"user,omitempty"`
	Password string   `json:"password,omitempty"`
	Types    []string `json:"types"`   // Album, EP, Single
	LinkURL  string   `json:"linkUrl"` // where dropwatch is reachable, for tap-to-open
	// Since is when pushing was switched on; earlier notifications are not
	// pushed, so enabling ntfy doesn't send the whole backlog.
	Since time.Time `json:"since"`
}

// NotificationTypes are the release types notifications can be filtered by.
var NotificationTypes = []string{"Album", "EP", "Single"}

func (n NtfyConfig) ServerURL() string {
	if n.Server == "" {
		return "https://ntfy.sh"
	}
	return strings.TrimRight(n.Server, "/")
}

func (n NtfyConfig) Wants(primaryType string) bool { return slices.Contains(n.Types, primaryType) }

type MPDConfig struct {
	Enabled  bool   `json:"enabled"`
	Address  string `json:"address"` // host[:port] or a unix socket path
	Password string `json:"password,omitempty"`
}

type SubsonicConfig struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
}

type MelodyConfig struct {
	Enabled  bool   `json:"enabled"`
	Address  string `json:"address"` // host[:port] or a unix socket path
	Password string `json:"password,omitempty"`
}

type DiscogsConfig struct {
	Token string `json:"token,omitempty"`
}

// Validate checks the enabled sources have what they need to connect.
func (c Config) Validate() error {
	if c.MPD.Enabled && strings.TrimSpace(c.MPD.Address) == "" {
		return fmt.Errorf("MPD needs an address")
	}
	if c.Subsonic.Enabled {
		u, err := url.Parse(c.Subsonic.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("Subsonic needs an http(s) URL")
		}
		if c.Subsonic.User == "" || c.Subsonic.Password == "" {
			return fmt.Errorf("Subsonic needs a user and password")
		}
	}
	if c.Melody.Enabled {
		if strings.TrimSpace(c.Melody.Address) == "" {
			return fmt.Errorf("melody needs an address")
		}
		if c.Melody.Password == "" && !strings.HasPrefix(c.Melody.Address, "/") {
			return fmt.Errorf("melody over the network needs its password")
		}
	}
	if c.Ntfy.Enabled {
		if c.Ntfy.Topic == "" || strings.ContainsAny(c.Ntfy.Topic, "/ ") {
			return fmt.Errorf("ntfy needs a topic name (no slashes or spaces)")
		}
		if u, err := url.Parse(c.Ntfy.ServerURL()); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("the ntfy server must be an http(s) URL")
		}
		if len(c.Ntfy.Types) == 0 {
			return fmt.Errorf("choose at least one release type to push")
		}
	}
	for _, t := range c.Ntfy.Types {
		if !slices.Contains(NotificationTypes, t) {
			return fmt.Errorf("unknown release type %q", t)
		}
	}
	if c.Sync.IntervalHours < 0 || c.Sync.IntervalHours > 168 {
		return fmt.Errorf("the sync interval must be between 1 and 168 hours, or off")
	}
	if c.Ntfy.LinkURL != "" {
		if u, err := url.Parse(c.Ntfy.LinkURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("the dropwatch address must be an http(s) URL")
		}
	}
	return nil
}

// PublicConfig is Config as the browser sees it: secrets replaced by whether
// they are set.
type PublicConfig struct {
	MPD struct {
		Enabled     bool   `json:"enabled"`
		Address     string `json:"address"`
		PasswordSet bool   `json:"passwordSet"`
	} `json:"mpd"`
	Subsonic struct {
		Enabled     bool   `json:"enabled"`
		URL         string `json:"url"`
		User        string `json:"user"`
		PasswordSet bool   `json:"passwordSet"`
	} `json:"subsonic"`
	Melody struct {
		Enabled     bool   `json:"enabled"`
		Address     string `json:"address"`
		PasswordSet bool   `json:"passwordSet"`
	} `json:"melody"`
	Discogs struct {
		TokenSet bool `json:"tokenSet"`
	} `json:"discogs"`
	Ntfy struct {
		Enabled     bool     `json:"enabled"`
		Server      string   `json:"server"`
		Topic       string   `json:"topic"`
		TokenSet    bool     `json:"tokenSet"`
		User        string   `json:"user"`
		PasswordSet bool     `json:"passwordSet"`
		Types       []string `json:"types"`
		LinkURL     string   `json:"linkUrl"`
	} `json:"ntfy"`
	Sync SyncConfig `json:"sync"`
}

func (c Config) Public() PublicConfig {
	var p PublicConfig
	p.MPD.Enabled, p.MPD.Address, p.MPD.PasswordSet = c.MPD.Enabled, c.MPD.Address, c.MPD.Password != ""
	p.Subsonic.Enabled, p.Subsonic.URL, p.Subsonic.User, p.Subsonic.PasswordSet = c.Subsonic.Enabled, c.Subsonic.URL, c.Subsonic.User, c.Subsonic.Password != ""
	p.Melody.Enabled, p.Melody.Address, p.Melody.PasswordSet = c.Melody.Enabled, c.Melody.Address, c.Melody.Password != ""
	p.Discogs.TokenSet = c.Discogs.Token != ""
	n := c.Ntfy
	p.Ntfy.Enabled, p.Ntfy.Server, p.Ntfy.Topic, p.Ntfy.User, p.Ntfy.LinkURL = n.Enabled, n.Server, n.Topic, n.User, n.LinkURL
	p.Ntfy.TokenSet, p.Ntfy.PasswordSet = n.Token != "", n.Password != ""
	p.Ntfy.Types = n.Types
	if p.Ntfy.Types == nil {
		p.Ntfy.Types = []string{}
	}
	p.Sync = c.Sync
	return p
}

// ConfigUpdate is a settings form submission. A section left out keeps its
// stored values; within a section, a nil secret keeps the stored one and an
// empty one clears it.
type ConfigUpdate struct {
	MPD *struct {
		Enabled  bool    `json:"enabled"`
		Address  string  `json:"address"`
		Password *string `json:"password"`
	} `json:"mpd"`
	Subsonic *struct {
		Enabled  bool    `json:"enabled"`
		URL      string  `json:"url"`
		User     string  `json:"user"`
		Password *string `json:"password"`
	} `json:"subsonic"`
	Melody *struct {
		Enabled  bool    `json:"enabled"`
		Address  string  `json:"address"`
		Password *string `json:"password"`
	} `json:"melody"`
	Discogs *struct {
		Token *string `json:"token"`
	} `json:"discogs"`
	Ntfy *struct {
		Enabled  bool     `json:"enabled"`
		Server   string   `json:"server"`
		Topic    string   `json:"topic"`
		Token    *string  `json:"token"`
		User     string   `json:"user"`
		Password *string  `json:"password"`
		Types    []string `json:"types"`
		LinkURL  string   `json:"linkUrl"`
	} `json:"ntfy"`
	Sync *SyncConfig `json:"sync"`
}

// Apply merges the update into the stored config.
func (u ConfigUpdate) Apply(c Config) Config {
	keep := func(old string, new *string) string {
		if new == nil {
			return old
		}
		return *new // passwords may legitimately contain spaces
	}
	if m := u.MPD; m != nil {
		c.MPD = MPDConfig{Enabled: m.Enabled, Address: strings.TrimSpace(m.Address), Password: keep(c.MPD.Password, m.Password)}
	}
	if m := u.Subsonic; m != nil {
		c.Subsonic = SubsonicConfig{Enabled: m.Enabled, URL: strings.TrimSpace(m.URL), User: strings.TrimSpace(m.User),
			Password: keep(c.Subsonic.Password, m.Password)}
	}
	if m := u.Melody; m != nil {
		c.Melody = MelodyConfig{Enabled: m.Enabled, Address: strings.TrimSpace(m.Address), Password: keep(c.Melody.Password, m.Password)}
	}
	if m := u.Discogs; m != nil {
		c.Discogs.Token = keep(c.Discogs.Token, m.Token)
	}
	if n := u.Ntfy; n != nil {
		c.Ntfy = NtfyConfig{Enabled: n.Enabled, Server: strings.TrimSpace(n.Server), Topic: strings.TrimSpace(n.Topic),
			Token: keep(c.Ntfy.Token, n.Token), User: strings.TrimSpace(n.User), Password: keep(c.Ntfy.Password, n.Password),
			Types: n.Types, LinkURL: strings.TrimRight(strings.TrimSpace(n.LinkURL), "/"), Since: c.Ntfy.Since}
	}
	if u.Sync != nil {
		c.Sync = *u.Sync
	}
	return c
}

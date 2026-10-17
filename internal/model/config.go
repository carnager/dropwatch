package model

import (
	"fmt"
	"net/url"
	"strings"
)

// Config is everything set up in the web UI. Secrets (passwords, tokens) are
// stored here but never sent back to the browser; see Public.
type Config struct {
	MPD      MPDConfig      `json:"mpd"`
	Subsonic SubsonicConfig `json:"subsonic"`
	Melody   MelodyConfig   `json:"melody"`
	Discogs  DiscogsConfig  `json:"discogs"`
}

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
}

func (c Config) Public() PublicConfig {
	var p PublicConfig
	p.MPD.Enabled, p.MPD.Address, p.MPD.PasswordSet = c.MPD.Enabled, c.MPD.Address, c.MPD.Password != ""
	p.Subsonic.Enabled, p.Subsonic.URL, p.Subsonic.User, p.Subsonic.PasswordSet = c.Subsonic.Enabled, c.Subsonic.URL, c.Subsonic.User, c.Subsonic.Password != ""
	p.Melody.Enabled, p.Melody.Address, p.Melody.PasswordSet = c.Melody.Enabled, c.Melody.Address, c.Melody.Password != ""
	p.Discogs.TokenSet = c.Discogs.Token != ""
	return p
}

// ConfigUpdate is a settings form submission. A nil secret keeps the stored
// value; an empty one clears it.
type ConfigUpdate struct {
	MPD struct {
		Enabled  bool    `json:"enabled"`
		Address  string  `json:"address"`
		Password *string `json:"password"`
	} `json:"mpd"`
	Subsonic struct {
		Enabled  bool    `json:"enabled"`
		URL      string  `json:"url"`
		User     string  `json:"user"`
		Password *string `json:"password"`
	} `json:"subsonic"`
	Melody struct {
		Enabled  bool    `json:"enabled"`
		Address  string  `json:"address"`
		Password *string `json:"password"`
	} `json:"melody"`
	Discogs struct {
		Token *string `json:"token"`
	} `json:"discogs"`
}

// Apply merges the update into the stored config.
func (u ConfigUpdate) Apply(c Config) Config {
	keep := func(old string, new *string) string {
		if new == nil {
			return old
		}
		return *new // passwords may legitimately contain spaces
	}
	c.MPD = MPDConfig{Enabled: u.MPD.Enabled, Address: strings.TrimSpace(u.MPD.Address), Password: keep(c.MPD.Password, u.MPD.Password)}
	c.Subsonic = SubsonicConfig{Enabled: u.Subsonic.Enabled, URL: strings.TrimSpace(u.Subsonic.URL), User: strings.TrimSpace(u.Subsonic.User),
		Password: keep(c.Subsonic.Password, u.Subsonic.Password)}
	c.Melody = MelodyConfig{Enabled: u.Melody.Enabled, Address: strings.TrimSpace(u.Melody.Address), Password: keep(c.Melody.Password, u.Melody.Password)}
	c.Discogs.Token = keep(c.Discogs.Token, u.Discogs.Token)
	return c
}

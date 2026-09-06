// Package mpdclient speaks just enough of the MPD protocol to read the
// library: which album artists exist and which albums they have, including
// MusicBrainz release-group IDs when the server exposes that tag.
package mpdclient

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

type Album struct {
	Title string
	MBID  string // MUSICBRAINZ_RELEASEGROUPID, if tagged and configured in mpd
}

type Artist struct {
	Name   string
	Albums []Album
}

type Client struct {
	conn net.Conn
	r    *bufio.Reader
}

// Dial connects and authenticates. addr defaults to port 6600 when none is
// given; a path starting with "/" is treated as a unix socket.
func Dial(addr, password string) (*Client, error) {
	network := "tcp"
	if strings.HasPrefix(addr, "/") {
		network = "unix"
	} else if !strings.Contains(addr, ":") {
		addr += ":6600"
	}
	conn, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn)}
	greeting, err := c.r.ReadString('\n')
	if err != nil || !strings.HasPrefix(greeting, "OK MPD") {
		conn.Close()
		return nil, fmt.Errorf("mpd: unexpected greeting %q (err: %v)", strings.TrimSpace(greeting), err)
	}
	if password != "" {
		if _, err := c.command("password " + quote(password)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("mpd: auth failed: %w", err)
		}
	}
	return c, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// command sends one command and returns the response lines up to (not
// including) the terminating OK. An ACK response becomes an error.
func (c *Client) command(cmd string) ([]string, error) {
	c.conn.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := fmt.Fprintf(c.conn, "%s\n", cmd); err != nil {
		return nil, err
	}
	var lines []string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\n")
		if line == "OK" {
			return lines, nil
		}
		if strings.HasPrefix(line, "ACK ") {
			return nil, fmt.Errorf("mpd: %s", line)
		}
		lines = append(lines, line)
	}
}

// Library returns all album artists with their albums. It first tries a
// grouped listing that includes release-group MBIDs; servers without that tag
// (not in mpd.conf's metadata_to_use, or untagged files) fall back to plain
// album titles.
func (c *Client) Library() ([]Artist, error) {
	lines, err := c.command("list musicbrainz_releasegroupid group album group albumartist")
	if err != nil {
		lines, err = c.command("list album group albumartist")
		if err != nil {
			return nil, err
		}
	}
	var (
		out   []Artist
		index = map[string]int{}
		cur   = -1
	)
	for _, line := range lines {
		tag, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch strings.ToLower(tag) {
		case "albumartist":
			if value == "" {
				cur = -1
				continue
			}
			i, seen := index[value]
			if !seen {
				i = len(out)
				out = append(out, Artist{Name: value})
				index[value] = i
			}
			cur = i
		case "album":
			if cur >= 0 && value != "" {
				out[cur].Albums = append(out[cur].Albums, Album{Title: value})
			}
		case "musicbrainz_releasegroupid":
			if cur >= 0 && value != "" {
				if n := len(out[cur].Albums); n > 0 && out[cur].Albums[n-1].MBID == "" {
					out[cur].Albums[n-1].MBID = value
				}
			}
		}
	}
	return out, nil
}

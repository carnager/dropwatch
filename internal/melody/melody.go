// Package melody reads the library from a melody (melody-next) engine over its
// JSON-lines protocol: one JSON object per line, requests matched to responses
// by id, unsolicited events interleaved and ignored.
package melody

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"dropwatch/internal/mpdclient"
)

// DefaultPort is the engine's TCP protocol port.
const DefaultPort = "6603"

type Client struct {
	addr     string
	password string
}

// New returns a client for addr (host[:port], or a unix socket path starting
// with "/", which needs no password).
func New(addr, password string) *Client {
	return &Client{addr: addr, password: password}
}

type conn struct {
	net.Conn
	r    *bufio.Reader
	next int
	stop func() bool
}

func (cn *conn) Close() error {
	cn.stop()
	return cn.Conn.Close()
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Client) dial(ctx context.Context) (*conn, error) {
	network, addr := "tcp", c.addr
	if strings.HasPrefix(addr, "/") {
		network = "unix"
	} else if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, DefaultPort)
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	nc, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("melody: %w", err)
	}
	// Unblock reads when the caller gives up.
	cn := &conn{Conn: nc, r: bufio.NewReader(nc), stop: context.AfterFunc(ctx, func() { nc.SetDeadline(time.Now()) })}
	if c.password != "" {
		var res struct {
			Authenticated bool `json:"authenticated"`
		}
		if err := cn.call("session.authenticate", map[string]any{"password": c.password}, &res); err != nil {
			cn.Close()
			return nil, err
		}
		if !res.Authenticated {
			cn.Close()
			return nil, fmt.Errorf("melody: authentication refused")
		}
	}
	return cn, nil
}

func (cn *conn) call(method string, params any, out any) error {
	cn.next++
	id := cn.next
	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err := cn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("melody: %s: %w", method, err)
	}
	for {
		// A whole library can arrive as one line, so no line-length cap.
		line, err := cn.r.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("melody: %s: %w", method, err)
		}
		var msg struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("melody: %s: %w", method, err)
		}
		if msg.ID == nil || *msg.ID != id {
			continue // event, or a reply to something else
		}
		if msg.Error != nil {
			return fmt.Errorf("melody: %s: %s (%s)", method, msg.Error.Message, msg.Error.Code)
		}
		return json.Unmarshal(msg.Result, out)
	}
}

// Ping verifies connectivity and the password.
func (c *Client) Ping(ctx context.Context) error {
	cn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cn.Close()
	var info map[string]any
	return cn.call("engine.info", map[string]any{}, &info)
}

// Library returns every album artist with their albums. An album's MBID is
// its MusicBrainz *release* ID, which the engine encodes into the album key
// as "mbid:<id>"; untagged albums have none.
func (c *Client) Library(ctx context.Context) ([]mpdclient.Artist, error) {
	cn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer cn.Close()
	// The engine re-sorts the whole library for every page, so one page of
	// its maximum size is the fastest way to read everything.
	const pageSize = 100000
	var out []mpdclient.Artist
	index := map[string]int{}
	for offset := 0; ; {
		var page struct {
			Entries []struct {
				Key    string `json:"key"`
				Artist string `json:"artist"`
				Album  string `json:"album"`
			} `json:"entries"`
			More bool `json:"more"`
		}
		if err := cn.call("catalogue.query", map[string]any{"kind": 1, "offset": offset, "limit": pageSize,
			"fields": []string{"artist", "album"}}, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			if e.Artist == "" || e.Album == "" {
				continue
			}
			i, ok := index[e.Artist]
			if !ok {
				i = len(out)
				index[e.Artist] = i
				out = append(out, mpdclient.Artist{Name: e.Artist})
			}
			out[i].Albums = append(out[i].Albums, mpdclient.Album{Title: e.Album, MBID: releaseMBID(e.Key)})
		}
		offset += len(page.Entries)
		if !page.More || len(page.Entries) == 0 {
			return out, nil
		}
	}
}

func releaseMBID(key string) string {
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return ""
	}
	id, ok := strings.CutPrefix(string(raw), "mbid:")
	if !ok {
		return ""
	}
	return id
}

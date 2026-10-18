// Package ntfy publishes messages to an ntfy server (https://ntfy.sh or
// self-hosted) using its JSON publishing API.
package ntfy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Message struct {
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
	Priority int      `json:"priority,omitempty"`
}

type Client struct {
	server, topic         string
	token, user, password string
	http                  *http.Client
}

// New returns a client for one topic. A token is sent as a bearer token;
// otherwise user and password, if set, as basic auth.
func New(server, topic, token, user, password string) *Client {
	return &Client{server: server, topic: topic, token: token, user: user, password: password,
		http: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) Publish(ctx context.Context, m Message) error {
	body, err := json.Marshal(struct {
		Topic string `json:"topic"`
		Message
	}{c.topic, m})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.user != "":
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Keep the request URL (server and topic) out of the message.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("ntfy: %s (%s)", e.Error, resp.Status)
		}
		return fmt.Errorf("ntfy: server returned %s", resp.Status)
	}
	return nil
}

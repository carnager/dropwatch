package melody

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

// fakeEngine answers one connection: it checks the password, interleaves an
// event before each reply, and serves the library in pages of two albums.
func fakeEngine(t *testing.T, password string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	key := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	albums := []map[string]any{
		{"key": key("mbid:rel-1"), "artist": "Boards of Canada", "album": "Geogaddi"},
		{"key": key("16:Boards of Canada7:Twoism:/music/boc"), "artist": "Boards of Canada", "album": "Twoism"},
		{"key": key("mbid:rel-3"), "artist": "Autechre", "album": "Tri Repetae"},
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r, w := bufio.NewReader(c), json.NewEncoder(c)
				authed := false
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						ID     int            `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					json.Unmarshal(line, &req)
					w.Encode(map[string]any{"event": "playback.position", "data": map[string]any{}})
					switch {
					case req.Method == "session.authenticate":
						if req.Params["password"] != password {
							w.Encode(map[string]any{"id": req.ID, "error": map[string]any{"code": "unauthorized", "message": "wrong password"}})
							return
						}
						authed = true
						w.Encode(map[string]any{"id": req.ID, "result": map[string]any{"authenticated": true}})
					case !authed:
						w.Encode(map[string]any{"id": req.ID, "error": map[string]any{"code": "unauthorized", "message": "authenticate first"}})
					case req.Method == "engine.info":
						w.Encode(map[string]any{"id": req.ID, "result": map[string]any{"level": 15}})
					case req.Method == "catalogue.query":
						off := int(req.Params["offset"].(float64))
						end := min(off+2, len(albums))
						w.Encode(map[string]any{"id": req.ID, "result": map[string]any{"entries": albums[off:end], "more": end < len(albums)}})
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestLibrary(t *testing.T) {
	addr := fakeEngine(t, "secret")
	lib, err := New(addr, "secret").Library(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lib) != 2 || lib[0].Name != "Boards of Canada" || len(lib[0].Albums) != 2 || lib[1].Name != "Autechre" {
		t.Fatalf("library = %+v", lib)
	}
	if lib[0].Albums[0].MBID != "rel-1" || lib[0].Albums[1].MBID != "" || lib[1].Albums[0].MBID != "rel-3" {
		t.Fatalf("release MBIDs = %+v", lib)
	}
}

func TestWrongPassword(t *testing.T) {
	addr := fakeEngine(t, "secret")
	err := New(addr, "nope").Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("err = %v", err)
	}
	if err := New(addr, "secret").Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOpenDumpLargeRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.ndjson")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Exceed the former 32 MiB scanner limit with a valid release record.
	parts := []string{`{"id":"large","title":"`, strings.Repeat("x", (32<<20)+1), "\"}\n", `{"id":"next","title":"Following release"}`}
	for _, part := range parts {
		if _, err := io.WriteString(f, part); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	sc, closeDump, err := openDump(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeDump(); err != nil {
			t.Error(err)
		}
	})
	var ids []string
	for sc.Scan() {
		var release struct{ ID string }
		if err := json.Unmarshal(sc.Bytes(), &release); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, release.ID)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"large", "next"}) {
		t.Fatalf("release IDs = %v, want [large next]", ids)
	}
}

func TestDumpScannerLines(t *testing.T) {
	for _, input := range []string{"", "\n", "\r\n", "first\nsecond\n", "first\r\n\r\nlast", "invalid JSON\n{}\n", "last\r"} {
		t.Run(input, func(t *testing.T) {
			// Preserve ScanLines behavior for blank lines, malformed records,
			// CRLF, and a final record without a newline.
			want := bufio.NewScanner(strings.NewReader(input))
			got := &dumpScanner{reader: bufio.NewReader(strings.NewReader(input))}
			for want.Scan() {
				if !got.Scan() {
					t.Fatalf("missing line %q: %v", want.Text(), got.Err())
				}
				if string(got.Bytes()) != want.Text() {
					t.Fatalf("line = %q, want %q", got.Bytes(), want.Text())
				}
			}
			if got.Scan() || got.Scan() {
				t.Fatal("unexpected line after EOF")
			}
			if err := got.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failingDumpReader struct{ err error }

func (r failingDumpReader) Read([]byte) (int, error) { return 0, r.err }

func TestDumpScannerReadError(t *testing.T) {
	want := errors.New("dump read failed")
	reader := io.MultiReader(strings.NewReader("{}\n"), failingDumpReader{want})
	sc := &dumpScanner{reader: bufio.NewReader(reader)}
	if !sc.Scan() || string(sc.Bytes()) != "{}" {
		t.Fatal("missing record before read failure")
	}
	if sc.Scan() {
		t.Fatal("unexpected record after read failure")
	}
	if !errors.Is(sc.Err(), want) {
		t.Fatalf("error = %v, want %v", sc.Err(), want)
	}
}

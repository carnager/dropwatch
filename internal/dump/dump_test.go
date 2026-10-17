package dump

import (
	"context"
	"strings"
	"testing"
)

func TestReadGroupsLargeRecord(t *testing.T) {
	input := `{"id":"group","title":"Album","artist-credit":[{"artist":{"id":"tracked"}},{"artist":{"id":"tracked"}},{"artist":{"id":"other"}}],"ignored":"` + strings.Repeat("x", (32<<20)+1) + "\"}\r\n" +
		`{"id":"next","title":"Next","primary-type":"EP","artist-credit":[{"artist":{"id":"tracked"}}]}`
	groups, err := readGroups(context.Background(), strings.NewReader(input), map[string]bool{"tracked": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups["tracked"]) != 2 || groups["tracked"][1].ID != "next" {
		t.Fatalf("groups = %v", groups)
	}
}

func TestReadGroupsRejectsPartialOrWrongDump(t *testing.T) {
	for _, input := range []string{"", " \r\n", "{", "{}", `{"id":"group","artist-credit":[{"artist":{"id":"a"}}]}` + "\n{"} {
		if _, err := readGroups(context.Background(), strings.NewReader(input), map[string]bool{"a": true}, nil); err == nil {
			t.Errorf("accepted invalid dump %q", input)
		}
	}
}

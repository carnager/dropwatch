package subsonic

import (
	"context"
	"strings"
	"testing"
)

func TestErrorsDoNotLeakCredentials(t *testing.T) {
	err := New("http://127.0.0.1:1", "someone", "hunter2").Ping(context.Background())
	if err == nil {
		t.Fatal("expected a connection error")
	}
	for _, leak := range []string{"t=", "s=", "u=someone", "hunter2", "rest/ping?"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
}

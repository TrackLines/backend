package httpx

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestError(t *testing.T) {
	coded := Code("done_parent", "a done ticket can't take sub-tickets")
	send := func(accept string, err error) (int, string, string) {
		req := httptest.NewRequest("POST", "/", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		Error(rec, req, 409, err)
		return rec.Code, rec.Header().Get("Content-Type"), strings.TrimSpace(rec.Body.String())
	}
	// the web app asks for JSON and gets the code, also through wrapping
	if code, ct, body := send("application/json", fmt.Errorf("set parent: %w", coded)); code != 409 || ct != "application/json" ||
		body != `{"code":"done_parent","error":"set parent: a done ticket can't take sub-tickets"}` {
		t.Fatalf("json: %d %s %s", code, ct, body)
	}
	// uncoded: no code, the frontend falls back to the status
	if _, _, body := send("application/json, text/plain", errors.New("nope")); body != `{"error":"nope"}` {
		t.Fatalf("uncoded: %s", body)
	}
	// the MCP, the TUI and curl keep the plain text
	if _, ct, body := send("", coded); !strings.HasPrefix(ct, "text/plain") || body != "a done ticket can't take sub-tickets" {
		t.Fatalf("text: %s %s", ct, body)
	}
}

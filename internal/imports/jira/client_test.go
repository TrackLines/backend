package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TL-537: the client pages through both of Jira's list styles, waits out rate limits (Retry-After,
// else 1s, 2s, 4s…) and says when a token is refused.
func TestClient(t *testing.T) {
	limited := 2 // the next requests to /limited get 429
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "sam@acme.io" || p != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		switch r.URL.Path {
		case "/agile": // {values, isLast}, 2 a page, 5 in all
			var vs []int
			for i := start; i < min(start+2, 5); i++ {
				vs = append(vs, i)
			}
			json.NewEncoder(w).Encode(map[string]any{"values": vs, "isLast": start+2 >= 5})
		case "/comments": // {comments, total}, 3 a page, 7 in all
			var vs []int
			for i := start; i < min(start+3, 7); i++ {
				vs = append(vs, i)
			}
			json.NewEncoder(w).Encode(map[string]any{"comments": vs, "total": 7, "startAt": start})
		case "/limited":
			if limited > 0 {
				limited--
				if limited == 1 {
					w.Header().Set("Retry-After", "7")
				}
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, `{"ok": true}`)
		case "/rest/api/3/myself":
			fmt.Fprint(w, `{"displayName": "Sam Doe"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	var waits []time.Duration
	c := Client{Site: srv.URL, Email: "sam@acme.io", Token: "tok", HTTP: srv.Client(), sleep: func(d time.Duration) { waits = append(waits, d) }}
	ctx := context.Background()

	for path, want := range map[string]int{"/agile": 5, "/comments": 7} {
		key := "values"
		if path == "/comments" {
			key = "comments"
		}
		var got []int
		err := c.List(ctx, path, nil, key, func(raw json.RawMessage) error {
			var n int
			json.Unmarshal(raw, &n)
			got = append(got, n)
			return nil
		})
		if err != nil || len(got) != want || got[want-1] != want-1 {
			t.Fatalf("%s: %v %v", path, got, err)
		}
	}

	var ok struct{ OK bool }
	if err := c.Get(ctx, "/limited", nil, &ok); err != nil || !ok.OK {
		t.Fatalf("rate limited: %v %v", ok, err)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits: %v (Retry-After 7s, then the second try's 2s)", waits)
	}
	limited = 99
	if err := c.Get(ctx, "/limited", nil, &ok); err == nil {
		t.Fatal("gave up never")
	}

	if name, err := c.Myself(ctx); name != "Sam Doe" || err != nil {
		t.Fatalf("myself: %q %v", name, err)
	}
	bad := c
	bad.Token = "nope"
	if _, err := bad.Myself(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a bad token: %v", err)
	}
	gone := c
	gone.Site = "http://127.0.0.1:1"
	if _, err := gone.Myself(ctx); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("nowhere: %v", err)
	}
}

func TestSiteURL(t *testing.T) {
	for in, want := range map[string]string{
		"acme":                                   "https://acme.atlassian.net",
		"Acme.atlassian.net":                     "https://acme.atlassian.net",
		"https://acme.atlassian.net/jira/boards": "https://acme.atlassian.net",
		" https://my-team.atlassian.net ":        "https://my-team.atlassian.net",
		"http://acme.atlassian.net":              "",
		"https://evil.com":                       "",
		"https://acme.atlassian.net.evil.com":    "",
		"https://x@acme.atlassian.net":           "",
		"https://acme.atlassian.net:8443":        "",
		"https://a.b.atlassian.net":              "",
		"":                                       "",
	} {
		if got, _ := SiteURL(in); got != want {
			t.Errorf("SiteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

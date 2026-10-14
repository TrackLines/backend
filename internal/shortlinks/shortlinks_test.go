package shortlinks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Copy link gives a short link (TL-277): our own links only, each shortened once and reused; when the
// shortener fails or refuses, it says so (the browser then copies the full link).
func TestShortLinks(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var made atomic.Int32
	answer := `{"short":"yJjWqM","url":"x"}`
	tn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ URL string `json:"url"` }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/create" || r.Method != "POST" || !strings.HasPrefix(in.URL, "https://tracklin.es/") {
			http.Error(w, "bad", 400)
			return
		}
		made.Add(1)
		_, _ = w.Write([]byte(answer))
	}))
	defer tn.Close()
	s := System{DB: db, API: tn.URL, Base: "https://1tn.pw", Sites: []string{"https://tracklin.es", "http://localhost:3000"}}
	call := func(long string) (int, string) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"url":"`+long+`"}`))
		rec := httptest.NewRecorder()
		s.Create(rec, req)
		var out struct{ URL string `json:"url"` }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.URL
	}

	ticket := "https://tracklin.es/tickets/9205518f-7140-44cf-b155-4e9b8d6a2d48"
	if code, short := call(ticket); code != 200 || short != "https://1tn.pw/yJjWqM" {
		t.Fatalf("shorten: %d %s", code, short)
	}
	if code, short := call(ticket); code != 200 || short != "https://1tn.pw/yJjWqM" || made.Load() != 1 {
		t.Fatalf("second copy reuses it: %d %s, made %d", code, short, made.Load())
	}
	// not ours: refused, never sent to the shortener
	for _, bad := range []string{"https://evil.example/tickets/x", "https://tracklin.es.evil.example/x", "https://user@tracklin.es/x", "javascript:alert(1)", "tracklin.es/tickets/x", ""} {
		if code, _ := call(bad); code != 400 {
			t.Errorf("%q: %d", bad, code)
		}
	}
	if made.Load() != 1 {
		t.Fatalf("a refused link reached the shortener: %d", made.Load())
	}
	// the shortener refusing (an error in a 200) or down: 502, and nothing kept
	answer = `{"status":"invalid url"}`
	if code, _ := call("https://tracklin.es/boards/b1"); code != 502 {
		t.Fatalf("shortener error: %d", code)
	}
	tn.Close()
	if code, _ := call("https://tracklin.es/boards/b2"); code != 502 {
		t.Fatalf("shortener down: %d", code)
	}
	var kept int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM short_links WHERE url LIKE 'https://tracklin.es/boards/%'`).Scan(&kept)
	if kept != 0 {
		t.Fatalf("failed links kept: %d", kept)
	}
}

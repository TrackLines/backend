package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The API's description is public, signed in or not, as YAML and as JSON (TL-484).
func TestOpenAPIIsPublic(t *testing.T) {
	db, err := pgxpool.New(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := httptest.NewServer((&Service{DB: db}).Handler())
	defer srv.Close()
	get := func(path string) (*http.Response, string) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	res, body := get("/api/openapi.yaml")
	if res.StatusCode != 200 || !strings.HasPrefix(body, "openapi: 3") || !strings.Contains(res.Header.Get("Content-Type"), "yaml") {
		t.Fatalf("yaml: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, body = get("/api/openapi.json")
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if res.StatusCode != 200 || json.Unmarshal([]byte(body), &spec) != nil || spec.Paths["/openapi.json"] == nil {
		t.Fatalf("json: %d, describes itself: %v", res.StatusCode, spec.Paths["/openapi.json"] != nil)
	}
}

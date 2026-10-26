package internal

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The admin panel's routes answer 404 to anyone who isn't staff; the help answers need signing in,
// except the .po export the TUI's build fetches (TL-573/575).
func TestStaffRoutesAreHidden(t *testing.T) {
	db, err := pgxpool.New(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := httptest.NewServer((&Service{DB: db}).Handler())
	defer srv.Close()
	for path, want := range map[string]int{
		"/api/staff/me":      404,
		"/api/staff/stats":   404,
		"/api/staff/help":    404,
		"/api/staff/outages": 404,
		"/api/help":          401,
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, res.StatusCode, want)
		}
	}
}

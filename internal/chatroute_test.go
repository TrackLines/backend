package internal

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Google Chat's requests carry Google's bearer token, not a Clerk session: they must reach the Chat
// app's own check, not be refused by the Clerk middleware every other route sits behind (TL-289).
func TestGoogleChatRouteSkipsClerk(t *testing.T) {
	db, err := pgxpool.New(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := httptest.NewServer((&Service{DB: db}).Handler())
	defer srv.Close()
	google := "eyJhbGciOiJSUzI1NiIsImtpZCI6Imdvb2dsZSIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJodHRwczovL2FjY291bnRzLmdvb2dsZS5jb20ifQ.c2ln"
	req, _ := http.NewRequest("POST", srv.URL+"/api/webhooks/google-chat", strings.NewReader(`{"type":"MESSAGE"}`))
	req.Header.Set("Authorization", "Bearer "+google)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// no Chat app configured here: its handler answers 503; Clerk would have said 401
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("google chat route: %d", res.StatusCode)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
}

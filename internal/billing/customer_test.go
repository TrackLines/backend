package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL is provisioned by this package's TestMain.
func TestCustomer(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
	}
	ctx := context.Background()
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('c1', 'a@b.c') ON CONFLICT (clerk_id) DO UPDATE SET stripe_customer_id = NULL`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"cus_1"}`))
	}))
	defer srv.Close()
	s := Stripe{BaseURL: srv.URL}
	for range 2 {
		if id, err := Customer(ctx, db, s, "c1"); err != nil || id != "cus_1" {
			t.Fatalf("got %q %v", id, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("stripe called %d times, want 1 (second call should read the stored id)", calls.Load())
	}
	if paid, _ := IsPaid(ctx, db, "c1"); paid {
		t.Fatal("new customer reported as paid")
	}
}

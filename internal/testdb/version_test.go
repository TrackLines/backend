package testdb

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }

// The container is the major version Image names.
func TestVersion(t *testing.T) {
	conn, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	major, _, _ := strings.Cut(strings.TrimPrefix(Image, "postgres:"), "-")
	var v string
	if err := conn.QueryRow(context.Background(), `SHOW server_version`).Scan(&v); err != nil || !strings.HasPrefix(v, major+".") {
		t.Fatalf("server_version %q, want %s.x: %v", v, major, err)
	}
}

package webhooks

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

// OpenDB migrates the package's database and returns a pool on it, plus a sealing box.
func OpenDB(t *testing.T) (*pgxpool.Pool, secrets.Box) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	box, err := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return db, box
}

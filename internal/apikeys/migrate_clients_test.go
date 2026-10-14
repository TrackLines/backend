package apikeys

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Existing agent keys move to the new model (TL-269): each gets the client its name points to, or
// "other" labelled with its old name. Names stay, since work is recorded under them. Script keys are untouched.
func TestExistingKeysGetClients(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	if err := m.Migrate(60); err != nil {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id = 'mig1'; INSERT INTO users (clerk_id, email) VALUES ('mig1', 'm@i.g')`); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"Claude": "ai", "claude desktop": "ai", "Codex bot": "ai", "Cursor": "ai", "Copilot": "ai", "gemini": "ai", "my-bot": "ai", "deploy": "service"}
	for name, kind := range keys {
		if _, err := db.Exec(ctx, `INSERT INTO api_keys (owner_clerk_id, org_id, name, kind, prefix, hash) VALUES ('mig1', 'org_mig', $1, $2, 'tl_x', $3)`, name, kind, []byte("mig-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{ // name -> client, label
		"Claude": {"claude-code", ""}, "claude desktop": {"claude-desktop", ""}, "Codex bot": {"codex", ""}, "Cursor": {"cursor", ""},
		"Copilot": {"vscode", ""}, "gemini": {"gemini", ""}, "my-bot": {"other", "my-bot"}, "deploy": {"", ""},
	}
	for name, w := range want {
		var client, label *string
		var kept string
		if err := db.QueryRow(ctx, `SELECT name, client, label FROM api_keys WHERE org_id = 'org_mig' AND name = $1`, name).Scan(&kept, &client, &label); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if value(client) != w[0] || value(label) != w[1] {
			t.Errorf("%s: client=%q label=%q, want %q %q", name, value(client), value(label), w[0], w[1])
		}
		if client != nil && DisplayName(*client, value(label)) == "" {
			t.Errorf("%s: no display name", name)
		}
	}
}

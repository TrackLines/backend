package comments

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/notifications"
)

// @mentions in a comment tell the people mentioned (not the author).
func TestMentionNotifications(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'mn1'; DELETE FROM projects WHERE owner_clerk_id = 'mn1'`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('mn1', 'Mentions', 'MN') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'user_ava', 'Fix login', 0) RETURNING id`, pid).Scan(&tid)
	people := func(context.Context, string) ([]notifications.Person, error) {
		return []notifications.Person{{ID: "user_ava", Names: []string{"Ava Smith", "Ava"}}, {ID: "user_bo", Names: []string{"Bo"}}}, nil
	}
	h := NewSystem(db).WithMentions(notifications.Store{DB: db}, people)
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"body":"@Ava can you look? @Bo FYI (me@bo.com)"}`)).WithContext(auth.WithAPIKeyUser(ctx, "mn1", "user_bo"))
	req.SetPathValue("id", tid)
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != 201 {
		t.Fatalf("comment: %d %s", rec.Code, rec.Body)
	}
	var who, text string
	if err := db.QueryRow(ctx, `SELECT string_agg(user_id, ','), min(text) FROM notifications WHERE org_id = 'mn1' AND kind = 'mention'`).Scan(&who, &text); err != nil {
		t.Fatal(err)
	}
	if who != "user_ava" || text != "mentioned you on MN-1: Fix login" {
		t.Fatalf("mentions: %q %q", who, text) // Bo wrote it, so isn't told
	}
}

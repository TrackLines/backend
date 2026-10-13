package comments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// GET /api/tickets/{id}/comments: the conversation oldest first, replies linked to their parent;
// another org gets 404.
func TestListHandler(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'org_cl1'`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_cl1', 'Talk') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 'chatty', 0) RETURNING id`, pid).Scan(&tid)
	h := NewSystem(db)
	call := func(org, method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(auth.WithAPIKey(ctx, "user_cl", org, "Ava", ""))
		req.SetPathValue("id", tid)
		rec := httptest.NewRecorder()
		if method == "POST" {
			h.Create(rec, req)
		} else {
			h.List(rec, req)
		}
		return rec
	}
	var first Comment
	_ = json.Unmarshal(call("org_cl1", "POST", `{"body":"first"}`).Body.Bytes(), &first)
	call("org_cl1", "POST", `{"body":"a reply","parent_comment_id":"`+first.ID+`"}`)

	rec := call("org_cl1", "GET", "")
	var list []Comment
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if len(list) != 2 || list[0].Body != "first" || list[1].ParentID == nil || *list[1].ParentID != first.ID || list[0].Author != "Ava" {
		t.Fatalf("conversation: %+v", list)
	}
	if rec := call("org_cl2", "GET", ""); rec.Code != 404 {
		t.Fatalf("another org: %d %s", rec.Code, rec.Body)
	}
}

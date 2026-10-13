package attachments

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// Attachment handlers: record an uploaded file (only real UploadThing files, not on done tickets),
// list, and delete (the record, then the file on UploadThing); never across orgs.
func TestAttachmentHandlers(t *testing.T) {
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
	const org, other = "org_at1", "org_at2"
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id IN ('org_at1', 'org_at2')`); err != nil {
		t.Fatal(err)
	}
	var pid, open, done string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Files') RETURNING id`, org).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 'open', 0) RETURNING id`, pid).Scan(&open)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position, resolved_at) VALUES ($1, 'x', 'done', 1, now()) RETURNING id`, pid).Scan(&done)

	// UploadThing: records which file keys it was asked to delete
	var mu sync.Mutex
	var deleted []string
	ut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			FileKeys []string `json:"fileKeys"`
		}
		_ = json.Unmarshal(body, &in)
		if r.URL.Path != "/v6/deleteFiles" || r.Header.Get("x-uploadthing-api-key") != "sk_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		deleted = append(deleted, in.FileKeys...)
		mu.Unlock()
	}))
	defer ut.Close()
	token := base64.StdEncoding.EncodeToString([]byte(`{"apiKey":"sk_test"}`))
	h := NewSystem(db)
	sweeper := Sweeper{DB: db, Files: UploadThing{Token: token, BaseURL: ut.URL}}

	as := func(o string) context.Context { return auth.WithAPIKey(ctx, "user_at", o, "Ava", "") }
	run := func(action string, c context.Context, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		map[string]func(){"create": func() { h.Create(rec, req) }, "list": func() { h.List(rec, req) }, "delete": func() { h.Delete(rec, req) }}[action]()
		return rec
	}
	expect := func(want int, action string, c context.Context, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := run(action, c, id, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s: %d %s, want %d", action, id, body, rec.Code, rec.Body, want)
		}
		return rec
	}
	file := func(key, link string) string {
		return `{"key":"` + key + `","url":"` + link + `","name":"shot.png","size":2048,"content_type":"image/png"}`
	}

	// create: only an UploadThing file whose URL matches its key; not on done tickets; own org only
	var a Attachment
	_ = json.Unmarshal(expect(201, "create", as(org), open, file("at-key-1", "https://app.ufs.sh/f/at-key-1")).Body.Bytes(), &a)
	if a.Name != "shot.png" || a.Size != 2048 || a.CreatedBy != "Ava" {
		t.Fatalf("created: %+v", a)
	}
	expect(400, "create", as(org), open, file("at-key-2", "https://evil.example/f/at-key-2"))
	expect(400, "create", as(org), open, file("someone-elses-key", "https://app.ufs.sh/f/at-key-2"))
	expect(400, "create", as(org), open, `{"key":"at-key-3","url":"https://app.ufs.sh/f/at-key-3","name":" "}`)
	expect(409, "create", as(org), done, file("at-key-4", "https://app.ufs.sh/f/at-key-4"))
	expect(404, "create", as(other), open, file("at-key-5", "https://app.ufs.sh/f/at-key-5"))

	// list: own org only
	var list []Attachment
	_ = json.Unmarshal(expect(200, "list", as(org), open, "").Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("list: %+v", list)
	}
	expect(404, "list", as(other), open, "")

	// delete: another org can't; then the record goes, and the next sweep deletes the file on UploadThing
	expect(404, "delete", as(other), a.ID, "")
	expect(204, "delete", as(org), a.ID, "")
	if _, err := sweeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(deleted, ",")
	mu.Unlock()
	if !strings.Contains(got, "at-key-1") {
		t.Fatalf("UploadThing deletes: %q", got)
	}
	_ = json.Unmarshal(expect(200, "list", as(org), open, "").Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("still listed: %+v", list)
	}
	expect(404, "delete", as(org), a.ID, "")
}

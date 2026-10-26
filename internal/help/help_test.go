package help

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

func setup(t *testing.T) System {
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
	return System{DB: db}
}

func call(h http.HandlerFunc, method, target, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	if i := strings.Index(target, "/help/"); i >= 0 {
		req.SetPathValue("id", strings.TrimPrefix(target[i:], "/help/"))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TL-575: every answer the frontend had, in all seven languages, and the panel reads them by locale.
func TestSeededAnswers(t *testing.T) {
	s := setup(t)
	var list []Localized
	rec := call(s.List, "GET", "/api/help?locale=nl", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 37 {
		t.Fatalf("%d answers, %v", len(list), err)
	}
	if list[0].Slug != "add-board" || list[0].Question != "Hoe voeg ik nog een bord toe?" || list[0].Href != "/projects/[project]/edit" || list[0].Pages[0] != "project" {
		t.Errorf("first: %+v", list[0])
	}
	var langs int
	_ = s.DB.QueryRow(context.Background(), `SELECT count(*) FROM help_answers WHERE question ?& array['en','nl','de','es','fr','pt','tlh'] AND answer ?& array['en','nl','de','es','fr','pt','tlh']`).Scan(&langs)
	if langs != 37 {
		t.Errorf("%d answers in every language", langs)
	}
	if rec := call(s.List, "GET", "/api/help?locale=xx", ""); !strings.Contains(rec.Body.String(), "How do I add another board?") {
		t.Error("an unknown locale isn't English")
	}
}

func TestStaffEditsAndThePanel(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	_, _ = s.DB.Exec(ctx, `DELETE FROM help_answers WHERE slug LIKE 'test-%'`)

	for body, want := range map[string]int{
		`{"slug":"Bad Slug","question":{"en":"q"},"answer":{"en":"a"}}`:                   400,
		`{"slug":"test-x","question":{"nl":"v"},"answer":{"en":"a"}}`:                      400, // no English
		`{"slug":"test-x","question":{"en":"q","xx":"?"},"answer":{"en":"a"}}`:             400,
		`{"slug":"test-x","question":{"en":"q"},"answer":{"en":"a"},"href":"https://evil"}`: 400,
		`{"slug":"test-x","question":{"en":"q"},"answer":{"en":"a"},"href":"//evil"}`:       400,
		`{"slug":"test-x","question":{"en":"q"},"answer":{"en":"a"},"pages":["nowhere"]}`:   400,
		`{"slug":"add-board","question":{"en":"q"},"answer":{"en":"a"}}`:                   409,
	} {
		if rec := call(s.Create, "POST", "/api/staff/help", body); rec.Code != want {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}

	rec := call(s.Create, "POST", "/api/staff/help", `{"slug":"test-quote","question":{"en":"Say \"hi\"\nthen?","nl":"Zeg \"hoi\"?"},"answer":{"en":"A \\ b"},"href":"/boards/[board]","pages":["board"],"published":true}`)
	var made Answer
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.Position != 37 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	hidden := call(s.Create, "POST", "/api/staff/help", `{"slug":"test-hidden","question":{"en":"Hidden?"},"answer":{"en":"yes"},"published":false}`)
	if hidden.Code != 201 {
		t.Fatal(hidden.Body)
	}

	// the panel: English where there's no Dutch, unpublished left out, a 304 for what the client has
	rec = call(s.List, "GET", "/api/help?locale=nl", "")
	if !strings.Contains(rec.Body.String(), `"answer":"A \\ b"`) || !strings.Contains(rec.Body.String(), `Zeg \"hoi\"?`) || strings.Contains(rec.Body.String(), "Hidden?") {
		t.Errorf("panel: %s", rec.Body)
	}
	etag := rec.Header().Get("ETag")
	if again := call(s.List, "GET", "/api/help?locale=nl", "", "If-None-Match", etag); again.Code != 304 || etag == "" {
		t.Errorf("unchanged: %d", again.Code)
	}
	if call(s.List, "GET", "/api/help?locale=de", "", "If-None-Match", etag).Code != 200 {
		t.Error("another language isn't the same answers")
	}
	staff := call(s.Staff, "GET", "/api/staff/help", "")
	if !strings.Contains(staff.Body.String(), "Hidden?") {
		t.Error("staff don't see unpublished answers")
	}

	// editing changes the ETag; a missing id is 404
	upd := call(s.Update, "PUT", "/api/staff/help/"+made.ID, `{"slug":"test-quote","question":{"en":"Changed?"},"answer":{"en":"a"},"published":true}`)
	if upd.Code != 200 || call(s.List, "GET", "/api/help?locale=nl", "", "If-None-Match", etag).Code != 200 {
		t.Errorf("update: %d", upd.Code)
	}
	if rec := call(s.Update, "PUT", "/api/staff/help/00000000-0000-0000-0000-000000000000", `{"slug":"test-none","question":{"en":"q"},"answer":{"en":"a"}}`); rec.Code != 404 {
		t.Errorf("missing: %d", rec.Code)
	}

	// reordering takes every id once, or changes nothing
	var ids []string
	rows, _ := s.DB.Query(ctx, `SELECT id::text FROM help_answers ORDER BY position DESC`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	for _, bad := range [][]string{ids[1:], append(ids[1:], ids[1]), append(ids[1:], "00000000-0000-0000-0000-000000000000")} {
		b, _ := json.Marshal(map[string][]string{"ids": bad})
		if rec := call(s.Order, "PUT", "/api/staff/help/order", string(b)); rec.Code != 400 {
			t.Errorf("bad order: %d", rec.Code)
		}
	}
	var first string
	_ = s.DB.QueryRow(ctx, `SELECT slug FROM help_answers ORDER BY position LIMIT 1`).Scan(&first)
	if first != "add-board" {
		t.Errorf("a refused order changed things: %s first", first)
	}
	b, _ := json.Marshal(map[string][]string{"ids": ids})
	if rec := call(s.Order, "PUT", "/api/staff/help/order", string(b)); rec.Code != 204 {
		t.Fatalf("order: %d %s", rec.Code, rec.Body)
	}
	_ = s.DB.QueryRow(ctx, `SELECT slug FROM help_answers ORDER BY position LIMIT 1`).Scan(&first)
	if first != "test-hidden" {
		t.Errorf("reversed: %s first", first)
	}

	if call(s.Delete, "DELETE", "/api/staff/help/"+made.ID, "").Code != 204 || call(s.Delete, "DELETE", "/api/staff/help/"+made.ID, "").Code != 404 {
		t.Error("delete")
	}
}

// The .po the TUI bakes in: a context per question and answer, escaped, "" where there's no translation.
func TestExport(t *testing.T) {
	s := setup(t)
	_, _ = s.DB.Exec(context.Background(), `DELETE FROM help_answers WHERE slug LIKE 'test-%'`)
	call(s.Create, "POST", "/api/staff/help", `{"slug":"test-po","question":{"en":"Say \"hi\"\nok?","nl":"Zeg \"hoi\"?"},"answer":{"en":"back\\slash"},"href":"/x","pages":["board","project"],"words":"w1 w2","published":true}`)
	rec := call(s.Export, "GET", "/api/help/export.po?locale=nl", "")
	po := rec.Body.String()
	for _, want := range []string{
		"\"Language: nl\\n\"",
		"#. href: /x\n#. fallback_href: \n#. pages: board project\n#. words: w1 w2\nmsgctxt \"test-po:question\"\nmsgid \"Say \\\"hi\\\"\\nok?\"\nmsgstr \"Zeg \\\"hoi\\\"?\"\n",
		"msgctxt \"test-po:answer\"\nmsgid \"back\\\\slash\"\nmsgstr \"\"\n",
		"msgctxt \"add-board:question\"\nmsgid \"How do I add another board?\"\nmsgstr \"Hoe voeg ik nog een bord toe?\"",
	} {
		if !strings.Contains(po, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "gettext") || strings.Count(po, "msgctxt") != 2*38 {
		t.Errorf("%s, %d messages", rec.Header().Get("Content-Type"), strings.Count(po, "msgctxt"))
	}
}

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
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'mn1'; DELETE FROM ticket_mentions WHERE org_id = 'mn1'; DELETE FROM projects WHERE owner_clerk_id = 'mn1'`); err != nil {
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

	// "tickets that mention me" outlives clearing the notification; newest mention first, one row per ticket
	var tid2 string
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'user_ava', 'Fix logout', 1) RETURNING id`, pid).Scan(&tid2)
	comment := func(ticket, body string) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"body":"`+body+`"}`)).WithContext(auth.WithAPIKeyUser(ctx, "mn1", "user_bo"))
		req.SetPathValue("id", ticket)
		h.Create(httptest.NewRecorder(), req)
	}
	comment(tid2, "@Ava Smith this one too")
	comment(tid, "@ava again")
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'mn1'`); err != nil {
		t.Fatal(err)
	}
	mine := func(user, query string) (int, []Mention) {
		req := httptest.NewRequest("GET", "/api/mentions"+query, nil).WithContext(auth.WithAPIKey(ctx, user, "mn1", "ui", "")) // the person, in org mn1
		rec := httptest.NewRecorder()
		h.Mentions(rec, req)
		var out []Mention
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, got := mine("user_ava", ""); code != 200 || len(got) != 2 || got[0].Key != "MN-1" || got[1].Key != "MN-2" ||
		got[0].Actor != "user_bo" || got[0].ProjectName != "Mentions" || got[0].CommentID == "" {
		t.Fatalf("ava's mentions: %d %+v", code, got)
	}
	if _, got := mine("user_bo", ""); len(got) != 1 || got[0].Key != "MN-1" {
		t.Fatalf("Bo mentioned himself, so it's on his list (TL-587): %+v", got)
	}
	if _, got := mine("user_ava", "?limit=1"); len(got) != 1 {
		t.Fatalf("limit: %+v", got)
	}
	if code, _ := mine("user_ava", "?limit=0"); code != 400 {
		t.Fatalf("bad limit: %d", code)
	}
	// hide_done leaves out done and closed tickets; the page endpoint pages them with a total
	if _, err := db.Exec(ctx, `UPDATE tickets SET resolved_at = now(), resolution = 'done' WHERE id = $1`, tid2); err != nil {
		t.Fatal(err)
	}
	if _, got := mine("user_ava", "?hide_done=true"); len(got) != 1 || got[0].Key != "MN-1" {
		t.Fatalf("hide_done: %+v", got)
	}
	if _, got := mine("user_ava", ""); len(got) != 2 || !got[1].Done {
		t.Fatalf("done tickets show by default, marked done: %+v", got)
	}
	paged := func(query string) (int, MentionsPage) {
		req := httptest.NewRequest("GET", "/api/mentions/page"+query, nil).WithContext(auth.WithAPIKey(ctx, "user_ava", "mn1", "ui", ""))
		rec := httptest.NewRecorder()
		h.PageMentions(rec, req)
		var out MentionsPage
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, p := paged("?per_page=1&page=2"); code != 200 || p.Total != 2 || p.Page != 2 || len(p.Mentions) != 1 || p.Mentions[0].Key != "MN-2" {
		t.Fatalf("page 2: %d %+v", code, p)
	}
	if _, p := paged("?per_page=1&hide_done=true"); p.Total != 1 || len(p.Mentions) != 1 || p.Mentions[0].Key != "MN-1" {
		t.Fatalf("page, hide_done: %+v", p)
	}
	if _, p := paged("?page=5"); len(p.Mentions) != 0 {
		t.Fatalf("past the end: %+v", p)
	}
	// status=open|done|all (TL-589); hide_done=true is the older status=open
	for q, want := range map[string]string{"?status=done": "MN-2", "?status=open": "MN-1", "?status=all": "MN-1,MN-2", "?status=open&hide_done=false": "MN-1"} {
		var keys []string
		_, p := paged(q)
		for _, m := range p.Mentions {
			keys = append(keys, m.Key)
		}
		if got := strings.Join(keys, ","); got != want || p.Total != len(keys) {
			t.Fatalf("%s: %s (total %d)", q, got, p.Total)
		}
	}
	if code, _ := mine("user_ava", "?status=closed"); code != 400 {
		t.Fatalf("bad status: %d", code)
	}
	for _, q := range []string{"?page=0", "?per_page=101", "?page=x", "?status=nope"} {
		if code, _ := paged(q); code != 400 {
			t.Fatalf("%s: %d", q, code)
		}
	}
	req = httptest.NewRequest("GET", "/api/mentions", nil).WithContext(auth.WithAPIKey(ctx, "user_ava", "other-org", "ui", ""))
	rec = httptest.NewRecorder()
	h.Mentions(rec, req)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("another org: %s", rec.Body)
	}
}

// TL-587: mentioning yourself watches a ticket without a notification; watchers hear about new
// comments (not the author, not who was just mentioned); muting stops that and keeps it listed.
func TestWatching(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'wa1'; DELETE FROM ticket_mentions WHERE org_id = 'wa1';
		DELETE FROM ticket_watch_mutes WHERE org_id = 'wa1'; DELETE FROM projects WHERE owner_clerk_id IN ('wa1', 'wa2')`); err != nil {
		t.Fatal(err)
	}
	var pid, tid, pid2, other string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('wa1', 'Watch', 'WA') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'user_ava', 'Fix login', 0) RETURNING id`, pid).Scan(&tid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('wa2', 'Other', 'OT') RETURNING id`).Scan(&pid2)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'user_x', 'Theirs', 0) RETURNING id`, pid2).Scan(&other)
	people := func(context.Context, string) ([]notifications.Person, error) {
		return []notifications.Person{{ID: "user_bob", Names: []string{"Bob"}}, {ID: "user_cy", Names: []string{"Cy"}}, {ID: "user_dee", Names: []string{"Dee"}}}, nil
	}
	h := NewSystem(db).WithMentions(notifications.Store{DB: db}, people)
	comment := func(user, body string) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"body":"`+body+`"}`)).WithContext(auth.WithAPIKeyUser(ctx, "wa1", user))
		req.SetPathValue("id", tid)
		rec := httptest.NewRecorder()
		h.Create(rec, req)
		if rec.Code != 201 {
			t.Fatalf("comment: %d %s", rec.Code, rec.Body)
		}
	}
	inbox := func(user string) []string {
		rows, _ := db.Query(ctx, `SELECT kind || ' ' || actor || ' ' || text FROM notifications WHERE org_id = 'wa1' AND user_id = $1 ORDER BY created_at, id`, user)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x string
			_ = rows.Scan(&x)
			out = append(out, x)
		}
		return out
	}
	mute := func(method, org, user, ticket string) int {
		req := httptest.NewRequest(method, "/", nil).WithContext(auth.WithAPIKey(ctx, user, org, "ui", ""))
		req.SetPathValue("ticket", ticket)
		rec := httptest.NewRecorder()
		h.Mute(rec, req)
		return rec.Code
	}
	listed := func(user string) []Mention {
		req := httptest.NewRequest("GET", "/api/mentions", nil).WithContext(auth.WithAPIKey(ctx, user, "wa1", "ui", ""))
		rec := httptest.NewRecorder()
		h.Mentions(rec, req)
		var out []Mention
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	// Bob watches by mentioning himself: listed, not told
	comment("user_bob", "@Bob watching this")
	if got := listed("user_bob"); len(got) != 1 || got[0].Muted {
		t.Fatalf("bob's self-mention: %+v", got)
	}
	if got := inbox("user_bob"); len(got) != 0 {
		t.Fatalf("no news for your own mention: %v", got)
	}
	// Cy mentions Dee: Dee is told she was mentioned (once, not also as a watcher); Bob hears of the comment
	comment("user_cy", "@Dee have a look")
	if got := inbox("user_dee"); len(got) != 1 || got[0] != "mention user_cy mentioned you on WA-1: Fix login" {
		t.Fatalf("dee: %v", got)
	}
	if got := inbox("user_bob"); len(got) != 1 || got[0] != "watching user_cy commented on WA-1: Fix login" {
		t.Fatalf("bob, watching: %v", got)
	}
	// Bob comments: Dee (a watcher now) hears; Bob isn't told about his own comment
	comment("user_bob", "on it")
	if got := inbox("user_dee"); len(got) != 2 || got[1] != "watching user_bob commented on WA-1: Fix login" {
		t.Fatalf("dee, watching: %v", got)
	}
	if got := inbox("user_bob"); len(got) != 1 {
		t.Fatalf("bob's own comment: %v", got)
	}
	// Bob mutes it: still listed, muted, and he isn't told; twice is fine
	if code := mute("PUT", "wa1", "user_bob", tid); code != 204 {
		t.Fatalf("mute: %d", code)
	}
	if code := mute("PUT", "wa1", "user_bob", tid); code != 204 {
		t.Fatalf("mute again: %d", code)
	}
	if got := listed("user_bob"); len(got) != 1 || !got[0].Muted {
		t.Fatalf("muted stays listed: %+v", got)
	}
	comment("user_cy", "still broken")
	if got := inbox("user_bob"); len(got) != 1 {
		t.Fatalf("muted: %v", got)
	}
	// unmuting watches it again, without a new mention
	if code := mute("DELETE", "wa1", "user_bob", tid); code != 204 {
		t.Fatalf("unmute: %d", code)
	}
	comment("user_cy", "fixed?")
	if got := inbox("user_bob"); len(got) != 2 || listed("user_bob")[0].Muted {
		t.Fatalf("unmuted: %v", got)
	}
	// another org's ticket, or no ticket at all: 404
	for _, ticket := range []string{other, "00000000-0000-0000-0000-000000000000", "nope"} {
		if code := mute("PUT", "wa1", "user_bob", ticket); code != 404 {
			t.Fatalf("mute %s: %d", ticket, code)
		}
	}
}

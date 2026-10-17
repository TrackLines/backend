package intake

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/tracklines/backend/internal/tickets"
)

func TestIntake(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id IN ('ik1', 'ik2')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('ik1', 'Secret Project', 'IK') RETURNING id`).Scan(&pid)
	h := NewSystem(db)
	member := auth.WithAPIKeyUser(ctx, "ik1", "user_ava")
	outsider := auth.WithAPIKeyUser(ctx, "ik2", "user_eve")
	call := func(c context.Context, handler http.HandlerFunc, method, body string, path map[string]string, ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		if ip != "" {
			req.Header.Set("X-Forwarded-For", "6.6.6."+ip[len(ip)-1:]+", "+ip) // left: whatever the client sent; right: what our edge saw
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	var st settings
	decode := func(rec *httptest.ResponseRecorder, v any) {
		t.Helper()
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}

	// closed by default; members open it and get a link; other orgs can't see or change it
	decode(call(member, h.Settings, "GET", "", map[string]string{"id": pid}, ""), &st)
	if st.Open || st.Token != nil {
		t.Fatalf("closed by default: %+v", st)
	}
	if rec := call(outsider, h.SetOpen, "PUT", `{"open":true}`, map[string]string{"id": pid}, ""); rec.Code != 404 {
		t.Fatalf("outsider opens: %d", rec.Code)
	}
	decode(call(member, h.SetOpen, "PUT", `{"open":true}`, map[string]string{"id": pid}, ""), &st)
	if !st.Open || st.Token == nil || len(*st.Token) < 20 {
		t.Fatalf("opened: %+v", st)
	}
	token := *st.Token
	public := context.Background()
	form := func(tok string) *httptest.ResponseRecorder {
		return call(public, h.Form, "GET", "", map[string]string{"token": tok}, "")
	}
	submit := func(body, ip string) int {
		return call(public, h.Submit, "POST", body, map[string]string{"token": token}, ip).Code
	}

	// the public form says it's open and nothing about the project
	if rec := form(token); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"open":true}` {
		t.Fatalf("form: %d %s", rec.Code, rec.Body)
	}
	if rec := form("nope"); rec.Code != 404 || strings.Contains(rec.Body.String(), "Secret") {
		t.Fatalf("unknown link: %d %s", rec.Code, rec.Body)
	}

	// validation, the honeypot, then a real request
	for _, bad := range []string{`{"type":"bug","title":""}`, `{"type":"question","title":"Hi"}`, `{"type":"bug","title":"` + strings.Repeat("x", 201) + `"}`} {
		if code := submit(bad, "1.1.1.1"); code != 400 {
			t.Fatalf("invalid %s: %d", bad[:20], code)
		}
	}
	if code := submit(`{"type":"bug","title":"Buy cheap pills","website":"spam.example"}`, "1.1.1.1"); code != 202 {
		t.Fatalf("honeypot: %d", code)
	}
	if code := submit(`{"type":"bug","title":"Login button does nothing","details":"On Safari 18","contact":"sam@example.com"}`, "1.1.1.1"); code != 202 {
		t.Fatalf("submit: %d", code)
	}
	// one sender gets 5 an hour; someone else still can
	for i := 0; i < 4; i++ {
		_ = submit(`{"type":"feature","title":"More","details":""}`, "1.1.1.1")
	}
	if code := submit(`{"type":"feature","title":"Sixth"}`, "1.1.1.1"); code != 429 {
		t.Fatalf("rate limit: %d", code)
	}
	if code := submit(`{"type":"feature","title":"Dark mode please"}`, "2.2.2.2"); code != 202 {
		t.Fatalf("other sender: %d", code)
	}

	// triage: members see pending ones (no bot entry), newest first; outsiders don't
	var list []Submission
	decode(call(member, h.List, "GET", "", map[string]string{"id": pid}, ""), &list)
	if len(list) != 6 || list[0].Title != "Dark mode please" || strings.Contains(list[len(list)-1].Title, "pills") {
		t.Fatalf("pending: %d %+v", len(list), list[0])
	}
	if rec := call(outsider, h.List, "GET", "", map[string]string{"id": pid}, ""); rec.Code != 404 {
		t.Fatalf("outsider list: %d", rec.Code)
	}
	bug := list[len(list)-1]
	if bug.Title != "Login button does nothing" || bug.Contact != "sam@example.com" {
		t.Fatalf("oldest: %+v", bug)
	}

	// promote: an unassigned backlog ticket with the details and where it came from; only once
	if rec := call(outsider, h.Promote, "POST", "", map[string]string{"id": bug.ID}, ""); rec.Code != 404 {
		t.Fatalf("outsider promote: %d", rec.Code)
	}
	var promoted Submission
	decode(call(member, h.Promote, "POST", "", map[string]string{"id": bug.ID}, ""), &promoted)
	if promoted.Status != "promoted" || promoted.TicketID == nil || promoted.TicketKey == nil || *promoted.DecidedBy != "user_ava" {
		t.Fatalf("promoted: %+v", promoted)
	}
	tk, err := tickets.Store{DB: db}.Get(ctx, "ik1", *promoted.TicketID)
	if err != nil || tk.BoardID != nil || tk.AssignedTo != nil || tk.Type != "bug" || tk.Title != "Login button does nothing" ||
		!strings.HasPrefix(tk.Description, "On Safari 18\n\n_Sent through the project's request form by `sam@example.com`._") || tk.CreatedBy != "user_ava" {
		t.Fatalf("ticket: %+v %v", tk, err)
	}
	if rec := call(member, h.Promote, "POST", "", map[string]string{"id": bug.ID}, ""); rec.Code != 409 {
		t.Fatalf("promote twice: %d", rec.Code)
	}
	// dismiss
	if rec := call(member, h.Dismiss, "POST", "", map[string]string{"id": list[0].ID}, ""); rec.Code != 204 {
		t.Fatalf("dismiss: %d", rec.Code)
	}
	decode(call(member, h.List, "GET", "", map[string]string{"id": pid}, ""), &list)
	if len(list) != 4 {
		t.Fatalf("pending after triage: %d", len(list))
	}
	var dismissed []Submission
	req := httptest.NewRequest("GET", "/?status=dismissed", nil).WithContext(member)
	req.SetPathValue("id", pid)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	decode(rec, &dismissed)
	if len(dismissed) != 1 || dismissed[0].Title != "Dark mode please" {
		t.Fatalf("dismissed: %+v", dismissed)
	}

	// rotating the link kills the old one; closing stops the form
	decode(call(member, h.SetOpen, "PUT", `{"open":true,"rotate":true}`, map[string]string{"id": pid}, ""), &st)
	if *st.Token == token || form(token).Code != 404 || form(*st.Token).Code != 200 {
		t.Fatal("rotate didn't replace the link")
	}
	decode(call(member, h.SetOpen, "PUT", `{"open":false}`, map[string]string{"id": pid}, ""), &st)
	if st.Open || st.Token != nil {
		t.Fatalf("closed: %+v", st)
	}
	token = ""
	if code := call(public, h.Submit, "POST", `{"type":"bug","title":"late"}`, map[string]string{"token": ""}, "3.3.3.3").Code; code != 404 {
		t.Fatalf("closed submit: %d", code)
	}
}

// TL-162: a burst of simultaneous requests from one sender can't get past the cap.
func TestIntakeBurst(t *testing.T) {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'ik3'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ik3', 'Burst') RETURNING id`).Scan(&pid)
	s := Store{DB: db}
	token, err := s.SetOpen(ctx, "ik3", pid, true, false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Submit(ctx, *token, "203.0.113.9", "bug", "same sender, all at once", "", "") == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var stored int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM intake_submissions WHERE project_id = $1`, pid).Scan(&stored)
	if accepted != PerSenderHour || stored != PerSenderHour {
		t.Fatalf("burst: %d accepted, %d stored (cap %d)", accepted, stored, PerSenderHour)
	}
}

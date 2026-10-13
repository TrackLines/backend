package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

func TestBugfixesPanel(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'org_bf'; DELETE FROM projects WHERE owner_clerk_id = 'org_bf';
		DELETE FROM org_integrations WHERE org_id = 'org_bf'; DELETE FROM org_admins WHERE org_id = 'org_bf';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_bf', 'admin', 'test')`); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))

	// a fake Bugfixes: checks the token, serves report 15098 in project 4703
	lastSeen := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	failing := false
	bfx := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.Header.Get("Authorization") != "Bearer bfx_token" || r.URL.Path != "/v1/projects/4703/reports/15098" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"report":          map[string]any{"title": "Error in RunAutoClose", "occurrences": 14, "status": "open", "last_seen": lastSeen, "levels": []string{"error"}},
			"static_analysis": map[string]any{"file": "src/internal/sprints/postgres.go", "line": 223},
		})
	}))
	defer bfx.Close()

	admins := organizations.Admins{DB: db}
	h := System{store: Store{DB: db, Box: box}, admins: admins, tickets: tickets.Store{DB: db}, now: time.Now}
	h.reports = BugfixesAPI{Store: h.store, Client: bfx.Client()} // trusts the test server; production uses externalClient

	session := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
			c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
			c.ActiveOrganizationID = "org_bf"
			return c
		}())
	}
	call := func(hf http.HandlerFunc, c context.Context, method, path, body string, values map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		for k, v := range values {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	bugfixes := map[string]string{"provider": "bugfixes"}

	// a ticket Bugfixes made, linked to its report; and one of our own
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_bf', 'p') RETURNING id`).Scan(&pid)
	ts := tickets.Store{DB: db}
	crash, _ := ts.CreateBacklog(ctx, "org_bf", pid, "bug", "Error in RunAutoClose", "")
	ours, _ := ts.CreateBacklog(ctx, "org_bf", pid, "task", "ours", "")
	if err := ts.SetExternal(ctx, "org_bf", crash.ID, &tickets.External{Source: "bugfixes", Project: "4703", ID: "15098", URL: "https://bugfix.es/projects/4703/reports/15098"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.SetExternal(ctx, "org_bf", ours.ID, &tickets.External{Source: "github", Project: "x", ID: "1"}); !errors.Is(err, tickets.ErrBadExternal) {
		t.Fatalf("unknown source: %v", err)
	}
	panel := func() TicketReport {
		t.Helper()
		rec := call(h.TicketReport, auth.WithAPIKeyUser(ctx, "org_bf", "claude"), "GET", "", "", map[string]string{"id": crash.ID})
		var out TicketReport
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("panel: %d %s", rec.Code, rec.Body)
		}
		return out
	}

	// not connected yet: still linked, just no details
	if p := panel(); p.Available || p.Reason != "not_connected" || p.URL == "" {
		t.Fatalf("not connected: %+v", p)
	}
	if rec := call(h.TicketReport, auth.WithAPIKeyUser(ctx, "org_bf", "claude"), "GET", "", "", map[string]string{"id": ours.ID}); rec.Code != 404 {
		t.Fatalf("our own ticket: %d", rec.Code)
	}

	// only signed-in org admins connect; the token is sealed and never shown
	body := `{"token":"bfx_token","base_url":"` + bfx.URL + `/"}`
	if rec := call(h.Connect, auth.WithAPIKey(ctx, "admin", "org_bf", "k", "ai"), "PUT", "", body, bugfixes); rec.Code != 403 {
		t.Fatalf("api key connect: %d", rec.Code)
	}
	if rec := call(h.Connect, session("member"), "PUT", "", body, bugfixes); rec.Code != 403 {
		t.Fatalf("member connect: %d", rec.Code)
	}
	if rec := call(h.Connect, session("admin"), "PUT", "", body, bugfixes); rec.Code != 204 {
		t.Fatalf("admin connect: %d %s", rec.Code, rec.Body)
	}
	var sealed []byte
	_ = db.QueryRow(ctx, `SELECT secret FROM org_integrations WHERE org_id = 'org_bf'`).Scan(&sealed)
	if bytes.Contains(sealed, []byte("bfx_token")) {
		t.Fatal("token stored in the clear")
	}
	for _, who := range []string{"admin", "member"} { // members see the status too (never the token)
		if rec := call(h.Connection, session(who), "GET", "", "", bugfixes); rec.Code != 200 || strings.Contains(rec.Body.String(), "bfx_token") || !strings.Contains(rec.Body.String(), `"connected":true`) {
			t.Fatalf("connection as %s: %d %s", who, rec.Code, rec.Body)
		}
	}

	// connected: the report's details; open + seen after the ticket was done = regressed
	p := panel()
	if !p.Available || p.Report == nil || p.Report.Occurrences != 14 || p.Report.Line != 223 || p.Regressed {
		t.Fatalf("details: %+v", p)
	}
	if err := ts.Resolve(ctx, "org_bf", crash.ID); err != nil {
		t.Fatal(err)
	}
	if p := panel(); !p.Regressed {
		t.Fatalf("regressed: %+v", p)
	}
	// Bugfixes down: linked, unavailable, no error
	failing = true
	if p := panel(); p.Available || p.Reason != "unavailable" {
		t.Fatalf("down: %+v", p)
	}
	failing = false
	// disconnect: back to not connected
	if rec := call(h.Disconnect, session("admin"), "DELETE", "", "", bugfixes); rec.Code != 204 {
		t.Fatalf("disconnect: %d", rec.Code)
	}
	if p := panel(); p.Reason != "not_connected" {
		t.Fatalf("after disconnect: %+v", p)
	}

	// integrations create backlog tickets with their reference (API keys only)
	th := tickets.NewSystem(db)
	create := func(c context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"title":"NPE","type":"bug","external":{"source":"bugfixes","project":"4703","id":"20001","url":"https://bugfix.es/r/20001"}}`)).WithContext(c)
		req.SetPathValue("id", pid)
		rec := httptest.NewRecorder()
		th.CreateBacklog(rec, req)
		return rec
	}
	if rec := create(auth.WithAPIKey(ctx, "bfx-agent", "org_bf", "bugfixes", "service")); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"external_source":"bugfixes"`) {
		t.Fatalf("bf create with reference: %d %s", rec.Code, rec.Body)
	}
	if rec := create(session("admin")); rec.Code != 403 {
		t.Fatalf("people can't set references: %d", rec.Code)
	}
}

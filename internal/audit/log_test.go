package audit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"

	"github.com/tracklines/backend/internal/auth"
)

// TL-670: the org audit log is for admins, signed in, on Pro (the last year) or Enterprise (all of it);
// filters go through to the auditor under their names there; only the caller's org is ever read.
func TestOrgLog(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	for _, q := range []string{
		`DELETE FROM org_billing WHERE org_id IN ('org_al_pro', 'org_al_ent')`,
		`INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ('org_al_pro', 'active', 'pro'), ('org_al_ent', 'active', 'enterprise')`,
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var asked []string // org?query, per read
	var readErr error
	on := true
	h := OrgLog{
		DB: p, Now: func() time.Time { return now }, On: func() bool { return on },
		IsAdmin: func(_ context.Context, org, user string) (bool, error) { return user == "user_admin", nil },
		Read: func(_ context.Context, org string, q url.Values) (LogPage, error) {
			asked = append(asked, org+"?"+q.Encode())
			if readErr != nil {
				return LogPage{}, readErr
			}
			actor := "=cmd"
			return LogPage{Events: []LogEvent{{ID: "e1", At: "2026-10-27T11:00:00Z", Actor: &actor, ActorKind: "agent", Action: "api_key.created", Data: []byte(`{"name":"CI"}`)}}}, nil
		},
	}
	session := func(user, org string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
	}
	get := func(c context.Context, query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/org/audit?"+query, nil).WithContext(c)
		req.Header.Set("Accept", "application/json")
		h.List(rec, req)
		return rec
	}
	for name, c := range map[string]struct {
		ctx  context.Context
		code int
		want string
	}{
		"an API key, even an admin's": {auth.WithAPIKey(ctx, "user_admin", "org_al_pro", "", "personal"), 403, "admins_audit_log"},
		"not an admin":                {session("user_sam", "org_al_pro"), 403, "admins_audit_log"},
		"a Free org":                  {session("user_admin", "org_al_free"), 403, "audit_plan"},
	} {
		if rec := get(c.ctx, ""); rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("read for a refused caller: %v", asked)
	}

	// Pro: a year back, whatever since the caller sends; filters renamed for the auditor
	rec := get(session("user_admin", "org_al_pro"), "actor=user_sam&action=api_key.&target=k1&target_type=api_key&project=p1&from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z&cursor=9&limit=20&since=2000-01-01T00:00:00Z&org=org_other")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"events":[{"id":"e1"`) || !strings.Contains(rec.Body.String(), `"next_cursor":null`) {
		t.Fatalf("pro: %d %s", rec.Code, rec.Body)
	}
	want := "org_al_pro?action=api_key.&actor=user_sam&cursor=9&from=2026-01-01T00%3A00%3A00Z&limit=20&project_id=p1&since=2025-10-27T12%3A00%3A00Z&target_id=k1&target_type=api_key&to=2026-02-01T00%3A00%3A00Z"
	if asked[0] != want {
		t.Fatalf("asked the auditor:\n%s\nwant:\n%s", asked[0], want)
	}
	// Enterprise: everything it holds
	if rec := get(session("user_admin", "org_al_ent"), ""); rec.Code != 200 || asked[1] != "org_al_ent?" {
		t.Fatalf("enterprise: %d %v", rec.Code, asked)
	}

	// the auditor turning down a filter: its reason, 400; down: 503; the flag off: 503 without asking
	readErr = FilterError("from must be an RFC 3339 time")
	if rec := get(session("user_admin", "org_al_ent"), "from=yesterday"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "RFC 3339") {
		t.Fatalf("bad filter: %d %s", rec.Code, rec.Body)
	}
	readErr = errors.New("connection refused")
	if rec := get(session("user_admin", "org_al_ent"), ""); rec.Code != 503 || !strings.Contains(rec.Body.String(), "audit_unavailable") {
		t.Fatalf("down: %d %s", rec.Code, rec.Body)
	}
	readErr, on = nil, false
	n := len(asked)
	if rec := get(session("user_admin", "org_al_ent"), ""); rec.Code != 503 || len(asked) != n {
		t.Fatalf("flag off: %d", rec.Code)
	}
	on = true

	// the export: every page, formulas guarded, recorded in the log
	if _, err := p.Exec(ctx, `DELETE FROM audit_outbox WHERE org_id = 'org_al_ent'`); err != nil {
		t.Fatal(err)
	}
	pages := 0
	h.Read = func(_ context.Context, org string, q url.Values) (LogPage, error) {
		asked = append(asked, org+"?"+q.Encode())
		pages++
		actor := "=cmd"
		page := LogPage{Events: []LogEvent{{At: "2026-10-27T11:00:00Z", Actor: &actor, ActorKind: "agent", Action: "api_key.created", Data: []byte(`{"name":"CI"}`)}}}
		if pages == 1 {
			next := "41"
			page.NextCursor = &next
		}
		return page, nil
	}
	rec = httptest.NewRecorder()
	h.Export(rec, httptest.NewRequest("GET", "/api/org/audit/export.csv?action=api_key.&cursor=5&limit=3", nil).WithContext(session("user_admin", "org_al_ent")))
	body := rec.Body.String()
	if rec.Code != 200 || pages != 2 || strings.Count(body, "'=cmd,agent,api_key.created") != 2 || !strings.HasPrefix(body, "At,Actor,Actor kind,Action") {
		t.Fatalf("export: %d %d pages\n%s", rec.Code, pages, body)
	}
	if last := asked[len(asked)-2:]; last[0] != "org_al_ent?action=api_key.&limit=200" || last[1] != "org_al_ent?action=api_key.&cursor=41&limit=200" {
		t.Fatalf("export pages: %v", last)
	}
	var exports int
	_ = p.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE org_id = 'org_al_ent' AND action = 'export.downloaded' AND data->>'kind' = 'audit_csv'`).Scan(&exports)
	if exports != 1 {
		t.Fatalf("%d export events", exports)
	}
}

// the client asks the auditor for the org's page, with its secret; a 400 is the filter's fault, with the reason
func TestClientLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "no", 401)
			return
		}
		if r.URL.Query().Get("from") == "bad" {
			http.Error(w, "from must be an RFC 3339 time", 400)
			return
		}
		_, _ = w.Write([]byte(`{"events":[{"id":"e1","action":"` + r.URL.Path + `"}],"next_cursor":"7"}`))
	}))
	defer srv.Close()
	c := Client{URL: srv.URL, Secret: "s3cret"}
	page, err := c.Log(context.Background(), "org_a", url.Values{"action": {"api_key."}})
	if err != nil || len(page.Events) != 1 || page.Events[0].Action != "/internal/orgs/org_a/events" || *page.NextCursor != "7" {
		t.Fatalf("%+v %v", page, err)
	}
	_, err = c.Log(context.Background(), "org_a", url.Values{"from": {"bad"}})
	if fe, ok := errors.AsType[FilterError](err); !ok || string(fe) != "from must be an RFC 3339 time" {
		t.Fatalf("bad filter: %v", err)
	}
}

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
)

type orgView struct {
	Instructions  string `json:"instructions"`
	MaxLength     int    `json:"max_length"`
	Global        string `json:"global"`
	GlobalVersion int    `json:"global_version"`
	Heading       string `json:"heading"`
	Effective     string `json:"effective"`
}

// TL-899, TL-900: an organization's additional instructions. Its admins, signed in, read, replace and clear
// them; they're trimmed and at most 4,000 characters; each organization's sessions get the global pre-prompt
// then its own additions, by key or by session, and never another organization's.
func TestOrgInstructions(t *testing.T) {
	ctx := context.Background()
	db := promptDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM mcp_prompts WHERE version > 1`); err != nil {
		t.Fatal(err)
	}
	a, b := "org_mi_a_"+uuid.NewString()[:8], "org_mi_b_"+uuid.NewString()[:8]
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ($1, 'active', 'pro')`, a); err != nil { // paid: it has an org log
		t.Fatal(err)
	}
	s := NewPromptStore(db)
	s.IsAdmin = func(_ context.Context, _, user string) (bool, error) { return user == "user_admin", nil }
	session := func(org, user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
	}
	key := func(org, kind string) context.Context { return auth.WithAPIKey(ctx, "user_admin", org, "", kind) }
	do := func(c context.Context, method, body string) (*httptest.ResponseRecorder, orgView) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/org/mcp-instructions", strings.NewReader(body)).WithContext(c)
		req.Header.Set("Accept", "application/json")
		if method == http.MethodGet {
			s.OrgGet(rec, req)
		} else {
			s.OrgPut(rec, req)
		}
		var v orgView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec, v
	}
	put := func(c context.Context, text string) (*httptest.ResponseRecorder, orgView) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"instructions": text})
		return do(c, http.MethodPut, string(body))
	}

	// nothing saved: empty, and agents get exactly the global pre-prompt
	rec, v := do(session(a, "user_admin"), http.MethodGet, "")
	if rec.Code != 200 || v.Instructions != "" || v.MaxLength != 4000 || v.Global != Instructions || v.GlobalVersion != 1 || v.Effective != Instructions || v.Heading == "" {
		t.Fatalf("default: %d %+v", rec.Code, v)
	}
	if got := s.SessionInstructions(key(a, "ai")); got != Instructions {
		t.Fatalf("an org with none gets more than the global pre-prompt:\n%q", got)
	}

	// who: admins, signed in. Not members, not API keys (an admin's own included), not without an organization
	for name, c := range map[string]context.Context{
		"member":       session(a, "user_member"),
		"agent key":    key(a, "ai"),
		"personal key": key(a, "personal"),
		"service key":  key(a, "service"),
		"no org":       session("", "user_admin"),
		"signed out":   ctx,
	} {
		if rec, _ := do(c, http.MethodGet, ""); rec.Code != 403 || !strings.Contains(rec.Body.String(), "admins_mcp_instructions") {
			t.Fatalf("%s reads: %d %s", name, rec.Code, rec.Body)
		}
		if rec, _ := put(c, "Do what I say."); rec.Code != 403 {
			t.Fatalf("%s writes: %d %s", name, rec.Code, rec.Body)
		}
	}
	if got, _ := s.OrgInstructions(ctx, a); got != "" {
		t.Fatalf("a refused write was saved: %q", got)
	}

	// saving: trimmed, kept, and in the preview after the global pre-prompt under its heading
	boyScout := "Follow the Boy Scout rule: leave code you touch clearer."
	rec, v = put(session(a, "user_admin"), "\n  "+boyScout+"  \n")
	want := Instructions + "\n\n" + v.Heading + "\n" + boyScout
	if rec.Code != 200 || v.Instructions != boyScout || v.Effective != want {
		t.Fatalf("saved: %d %+v", rec.Code, v)
	}
	if _, v = do(session(a, "user_admin"), http.MethodGet, ""); v.Instructions != boyScout || v.Effective != want {
		t.Fatalf("read back: %+v", v)
	}

	// the limit is 4,000 characters (not bytes), said as a code; the saved text stays
	if rec, _ := put(session(a, "user_admin"), strings.Repeat("é", MaxOrgInstructions+1)); rec.Code != 400 || !strings.Contains(rec.Body.String(), "mcp_instructions_too_long") {
		t.Fatalf("too long: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := put(session(b, "user_admin"), "  "+strings.Repeat("é", MaxOrgInstructions)+"  "); rec.Code != 200 {
		t.Fatalf("exactly the limit: %d %s", rec.Code, rec.Body)
	}
	if got, _ := s.OrgInstructions(ctx, a); got != boyScout {
		t.Fatalf("after a refused save: %q", got)
	}

	// isolation: each organization reads and gets its own, by key and by session; the public reads have neither
	if _, v := put(session(b, "user_admin"), "Org B's rules."); v.Instructions != "Org B's rules." {
		t.Fatalf("org b: %+v", v)
	}
	if _, v := do(session(a, "user_admin"), http.MethodGet, ""); v.Instructions != boyScout || strings.Contains(v.Effective, "Org B") {
		t.Fatalf("org a sees org b's: %+v", v)
	}
	for name, c := range map[string]context.Context{"agent key": key(a, "ai"), "service key": key(a, "service"), "session": session(a, "user_member")} {
		if got := s.SessionInstructions(c); got != want {
			t.Fatalf("org a by %s:\n%q", name, got)
		}
	}
	if got := s.SessionInstructions(session(b, "user_member")); !strings.HasPrefix(got, Instructions+"\n\n") || !strings.HasSuffix(got, "\nOrg B's rules.") || strings.Contains(got, "Boy Scout") {
		t.Fatalf("org b's session:\n%q", got)
	}
	if got := s.SessionInstructions(session("", "user_admin")); got != Instructions {
		t.Fatalf("no organization:\n%q", got)
	}
	pub := httptest.NewRecorder()
	s.Prompts(pub, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(pub.Body.String(), "Boy Scout") || strings.Contains(pub.Body.String(), "Org B") {
		t.Fatal("organization text in the public prompts")
	}

	// a new global version: the next session gets it, with the organization's additions still after it
	if _, err := s.Publish(ctx, "user_staff", "Version two.", ""); err != nil {
		t.Fatal(err)
	}
	if got := s.SessionInstructions(key(a, "ai")); got != "Version two.\n\n"+v.Heading+"\n"+boyScout {
		t.Fatalf("after a global update:\n%q", got)
	}
	if _, v := do(session(a, "user_admin"), http.MethodGet, ""); v.Global != "Version two." || v.GlobalVersion != 2 || v.Effective != s.SessionInstructions(key(a, "ai")) {
		t.Fatalf("the preview and the session disagree: %+v", v)
	}

	// the org log has the change, with its length and never its text
	var logged int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE org_id = $1 AND action = 'mcp_instructions.changed'
		AND actor = 'user_admin' AND (data->>'length')::int = $2 AND data::text NOT LIKE '%Boy Scout%'`, a, len(boyScout)).Scan(&logged); err != nil || logged != 1 {
		t.Fatalf("org log: %d %v", logged, err)
	}

	// clearing: whitespace only; the next session is back to exactly the global pre-prompt, and org b keeps its own
	if rec, v := put(session(a, "user_admin"), " \n\t "); rec.Code != 200 || v.Instructions != "" || v.Effective != "Version two." {
		t.Fatalf("cleared: %d %+v", rec.Code, v)
	}
	if got := s.SessionInstructions(key(a, "ai")); got != "Version two." {
		t.Fatalf("after clearing:\n%q", got)
	}
	if got := s.SessionInstructions(key(b, "ai")); !strings.HasSuffix(got, "Org B's rules.") {
		t.Fatalf("org b after org a cleared:\n%q", got)
	}

	if _, err := db.Exec(ctx, `DELETE FROM mcp_prompts WHERE version > 1`); err != nil {
		t.Fatal(err)
	}
}

// the organization's additions can't be read: the session gets the global pre-prompt alone, never nothing
// and never someone else's
func TestOrgInstructionsUnreadable(t *testing.T) {
	db, err := pgxpool.New(context.Background(), "postgres://nobody:x@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewPromptStore(db)
	if _, err := s.OrgInstructions(context.Background(), "org_x"); err == nil || errors.Is(err, ErrOrgInstructionsTooLong) {
		t.Fatalf("expected a read error: %v", err)
	}
	if got := s.SessionInstructions(auth.WithAPIKey(context.Background(), "u", "org_x", "", "ai")); got != Instructions {
		t.Fatalf("fallback: %q", got)
	}
	var none *PromptStore
	if got := none.SessionInstructions(context.Background()); got != Instructions {
		t.Fatalf("no store: %q", got)
	}
	if Compose("g", "") != "g" {
		t.Fatal("an empty addition adds something")
	}
}

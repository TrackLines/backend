package audit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

func paidOrg(t *testing.T, prefix string) string {
	t.Helper()
	org := prefix + uuid.NewString()[:8]
	if _, err := db(t).Exec(context.Background(), `INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ($1, 'active', 'pro')`, org); err != nil {
		t.Fatal(err)
	}
	return org
}

// logged is org's queued org-log events (not plan changes), as "action data".
func logged(t *testing.T, org string) []string {
	t.Helper()
	rows, err := db(t).Query(context.Background(), `SELECT action || ' ' || data::text FROM audit_outbox WHERE org_id = $1 AND action <> 'org.plan_changed' ORDER BY queued`, org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// TL-700: settings with before and after, the request form, embedding, repositories, integration
// settings, roadmap visibility, scrapped imports; nothing for a Free org; never a token or a secret
func TestAuditGapsQueued(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	run := func(org string) {
		t.Helper()
		var project, board, roadmap, imp string
		must := func(q string, args ...any) {
			t.Helper()
			if _, err := p.Exec(ctx, q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if err := p.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Shop') RETURNING id`, org).Scan(&project); err != nil {
			t.Fatal(err)
		}
		if err := p.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, $2, 'Web') RETURNING id`, project, org).Scan(&board); err != nil {
			t.Fatal(err)
		}
		must(`DELETE FROM audit_outbox WHERE org_id = $1`, org) // from here on
		must(`UPDATE boards SET estimate_scale = 'fibonacci', agent_review = true WHERE id = $1`, board)
		must(`UPDATE boards SET name = 'Site' WHERE id = $1`, board) // board.renamed (TL-669), not a settings change
		must(`UPDATE projects SET intake_token = 'tok-secret-1' WHERE id = $1`, project)
		must(`UPDATE projects SET intake_token = 'tok-secret-2' WHERE id = $1`, project)
		must(`UPDATE projects SET intake_token = NULL WHERE id = $1`, project)
		must(`INSERT INTO embed_origins (org, project_id, origin, created_by) VALUES ($1, $2, 'https://interviews.tools', 'user_admin')`, org, project)
		must(`DELETE FROM embed_origins WHERE project_id = $1`, project)
		must(`INSERT INTO embed_keys (org, project_id, key, label, created_by) VALUES ($1, $2, 'tle_' || $1, 'Marketing site', 'user_admin')`, org, project)
		must(`DELETE FROM embed_keys WHERE project_id = $1`, project)
		must(`INSERT INTO project_repos (project_id, org_id, provider, full_name) VALUES ($1, $2, 'github', 'acme/shop')`, project, org)
		must(`DELETE FROM project_repos WHERE project_id = $1`, project)
		must(`INSERT INTO chat_webhooks (org_id, url, kinds, updated_by) VALUES ($1, '\x00', '{done}', 'user_admin')`, org)
		must(`UPDATE chat_webhooks SET kinds = '{done,review}' WHERE org_id = $1`, org)
		must(`INSERT INTO org_integrations (org_id, provider, secret, updated_by, base_url) VALUES ($1, 'bugfixes', '\xdead', 'user_admin', 'https://bugs.example.com/api/secret-path')`, org)
		must(`UPDATE org_integrations SET base_url = 'https://bugs.other.io/x', secret = '\xbeef' WHERE org_id = $1`, org)
		if err := p.QueryRow(ctx, `INSERT INTO roadmaps (owner_clerk_id, project_id, title, visibility) VALUES ($1, $2, 'Plan', 'team') RETURNING id`, org, project).Scan(&roadmap); err != nil {
			t.Fatal(err)
		}
		must(`UPDATE roadmaps SET visibility = 'public' WHERE id = $1`, roadmap)
		if err := p.QueryRow(ctx, `INSERT INTO imports (org_id, source, plan, created_by) VALUES ($1, 'csv', '{}', 'user_admin') RETURNING id`, org).Scan(&imp); err != nil {
			t.Fatal(err)
		}
		must(`UPDATE imports SET state = 'scrapped' WHERE id = $1`, imp)
	}

	org := paidOrg(t, "org_gp_")
	run(org)
	got := logged(t, org)
	var actions []string
	for _, g := range got {
		actions = append(actions, strings.SplitN(g, " ", 2)[0])
	}
	want := []string{"board.settings_changed", "board.renamed", "project.request_form", "project.request_form", "project.request_form",
		"embed.site_added", "embed.site_removed", "embed.key_created", "embed.key_deleted", "repo.linked", "repo.unlinked",
		"integration.connected", "integration.updated", "integration.connected", "integration.updated", "roadmap.visibility_changed", "import.run"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("queued:\n%v\nwant:\n%v", actions, want)
	}
	all := strings.Join(got, "\n")
	for _, secret := range []string{"tok-secret", "secret-path", "dead", "beef"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q in the log:\n%s", secret, all)
		}
	}
	for _, want := range []string{
		`"fields": {"agent_review": {"to": true, "from": false}, "estimate_scale": {"to": "fibonacci", "from": "none"}}`,
		`"change": "opened"`, `"change": "reset"`, `"change": "closed"`,
		`"origin": "https://interviews.tools"`, `"label": "Marketing site"`, `"repo": "acme/shop"`,
		`"kinds": {"to": ["done", "review"], "from": ["done"]}`, `"base_url": {"to": "bugs.other.io", "from": "bugs.example.com"}`,
		`"to": "public", "from": "team"`, `"state": "scrapped"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s in:\n%s", want, all)
		}
	}

	free := "org_gf_" + uuid.NewString()[:8]
	run(free)
	if got := logged(t, free); len(got) != 0 {
		t.Fatalf("a Free org's changes were queued: %v", got)
	}
}

// TL-700: a change someone isn't allowed to make is recorded (with the route and the reason's code); one
// that goes through, a read, bad input and a signed-out caller aren't
func TestRefusedAttempts(t *testing.T) {
	ctx := context.Background()
	org := paidOrg(t, "org_rf_")
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/boards/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("as") {
		case "member":
			httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_or_leaders_only", "only organization admins or this board's team leaders can do this"))
		case "bad":
			http.Error(w, "bad", http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /api/org/audit", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	h := Refused(db(t), mux)(mux)
	call := func(c context.Context, method, path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader("{}")).WithContext(c)
		req.Header.Set("Accept", "application/json")
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	as := auth.WithAPIKey(ctx, "user_sam", org, "", "personal")
	if code := call(as, "PATCH", "/api/boards/b1?as=member"); code != 403 {
		t.Fatalf("the handler's answer changed: %d", code)
	}
	call(as, "PATCH", "/api/boards/b1")            // allowed: its own triggers record it
	call(as, "PATCH", "/api/boards/b1?as=bad")     // bad input
	call(as, "GET", "/api/org/audit")              // a read
	call(ctx, "PATCH", "/api/boards/b1?as=member") // nobody signed in
	got := logged(t, org)
	if len(got) != 1 || !strings.HasPrefix(got[0], "access.refused ") ||
		!strings.Contains(got[0], `"code": "admins_or_leaders_only"`) || !strings.Contains(got[0], `"route": "PATCH /api/boards/{id}"`) ||
		!strings.Contains(got[0], `"path": "/api/boards/b1"`) {
		t.Fatalf("recorded: %v", got)
	}
	var actor *string
	_ = db(t).QueryRow(ctx, `SELECT actor FROM audit_outbox WHERE org_id = $1 AND action = 'access.refused'`, org).Scan(&actor)
	if actor == nil || *actor != "user_sam" {
		t.Fatalf("by: %v", actor)
	}
}

// TL-700: a key that passes its expiry is recorded once; a revoked one isn't (its revoking was)
func TestExpiredKeysRecordedOnce(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	org := paidOrg(t, "org_ek_")
	for _, q := range []string{
		`INSERT INTO api_keys (owner_clerk_id, org_id, kind, name, prefix, hash, expires_at) VALUES ('user_a', $1, 'service', 'CI', 'tl_s_x', decode(md5($1 || 'a'), 'hex'), now() + interval '1 day')`,
		`INSERT INTO api_keys (owner_clerk_id, org_id, kind, name, prefix, hash, expires_at, revoked_at) VALUES ('user_a', $1, 'service', 'Old', 'tl_s_y', decode(md5($1 || 'b'), 'hex'), now() + interval '1 day', now())`,
		`UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE org_id = $1`,
		`DELETE FROM audit_outbox WHERE org_id = $1`,
	} {
		if _, err := p.Exec(ctx, q, org); err != nil {
			t.Fatal(q, err)
		}
	}
	for range 2 { // the second sweep finds nothing new
		if _, err := ExpireKeys(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got := logged(t, org)
	if len(got) != 1 || !strings.HasPrefix(got[0], "api_key.expired ") || !strings.Contains(got[0], `"name": "CI"`) {
		t.Fatalf("recorded: %v", got)
	}
}

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
	"sync"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

// fakeFlags is flags.gg's ticket flag API (flags-gg/orchestrator#242) over one agent with dev, staging
// and prod; down makes it answer 502, clash makes a create answer 409.
type fakeFlags struct {
	mu          sync.Mutex
	flags       map[string]*TicketFlag
	calls       []string
	down, clash bool
}

func (f *fakeFlags) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer flg_good" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if f.down {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	if r.URL.Path == "/integration/agents" {
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]any{{"project_id": "fp", "project_name": "Web", "agent_id": "agent-web", "agent_name": "web",
			"environments": []map[string]any{{"environment_id": "dev", "name": "dev", "level": 0}, {"environment_id": "prod", "name": "prod", "level": 1, "production": true}}},
			{"project_id": "fp", "project_name": "Web", "agent_id": "agent-apps", "agent_name": "apps"}}})
		return
	}
	key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/integration/tickets/"), "/flag")
	key = strings.TrimSuffix(key, "/flag/promote")
	tf := f.flags[key]
	answer := func(code int) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(f.flags[key]) }
	switch {
	case r.Method == http.MethodPut:
		if tf != nil {
			answer(200)
			return
		}
		if f.clash {
			http.Error(w, `{"error":"a flag with that name already exists"}`, http.StatusConflict)
			return
		}
		var in struct {
			AgentID string `json:"agent_id"`
			Name    string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.flags[key] = &TicketFlag{Ticket: key, Name: in.Name, AgentID: in.AgentID, Environments: []FlagEnvironment{
			{EnvironmentID: "dev", Name: "dev", Present: true}, {EnvironmentID: "staging", Name: "staging", Level: 1},
			{EnvironmentID: "prod", Name: "prod", Level: 2, Production: true}}}
		answer(201)
	case tf == nil && r.Method != http.MethodDelete:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	case r.Method == http.MethodGet:
		answer(200)
	case r.Method == http.MethodPatch:
		var in struct {
			EnvironmentID string `json:"environment_id"`
			FlagChange
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for i := range tf.Environments {
			if e := &tf.Environments[i]; e.EnvironmentID == in.EnvironmentID && e.Present {
				if in.Enabled != nil {
					e.Enabled = *in.Enabled
				}
				if in.Rollout != nil {
					e.Rollout = *in.Rollout
				}
				if in.Rules != nil {
					e.Rules = *in.Rules
				}
				answer(200)
				return
			}
		}
		http.Error(w, `{"error":"not in that environment"}`, http.StatusNotFound)
	case r.Method == http.MethodPost:
		for i := range tf.Environments {
			if !tf.Environments[i].Present {
				tf.Environments[i].Present = true
				if r.URL.Query().Get("to") != "production" {
					break
				}
			}
		}
		answer(200)
	case r.Method == http.MethodDelete:
		delete(f.flags, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeFlags) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func TestFlagsFollowTickets(t *testing.T) {
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
	const org = "org_fl"
	if _, err := db.Exec(ctx, `DELETE FROM flag_outbox WHERE org_id = 'org_fl'; DELETE FROM boards WHERE owner_clerk_id = 'org_fl'; DELETE FROM projects WHERE owner_clerk_id = 'org_fl';
		DELETE FROM org_integrations WHERE org_id = 'org_fl'; DELETE FROM org_admins WHERE org_id = 'org_fl';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_fl', 'admin', 'test')`); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	fake := &fakeFlags{flags: map[string]*TicketFlag{}}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()

	store := Store{DB: db, Box: box}
	api := FlagsAPI{Client: srv.Client()}
	h := System{store: store, admins: organizations.Admins{DB: db}, tickets: tickets.Store{DB: db}, now: time.Now}
	fl := h.Flags(api)
	ts := tickets.Store{DB: db}
	day := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	worker := FlagWorker{DB: db, Store: store, API: api, Tickets: ts, Now: func() time.Time { return day }}
	tick := func() { // until nothing is due: a ticket's jobs go one per tick, in order
		t.Helper()
		for {
			n, err := worker.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				return
			}
		}
	}
	sessionIn := func(user, in string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
			c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
			c.ActiveOrganizationID = in
			return c
		}())
	}
	session := func(user string) context.Context { return sessionIn(user, org) }
	call := func(hf http.HandlerFunc, c context.Context, method, body string, values map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		req.Header.Set("Accept", "application/json") // coded errors
		for k, v := range values {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	jobs := func() []string {
		t.Helper()
		rows, _ := db.Query(ctx, `SELECT action FROM flag_outbox WHERE org_id = $1 ORDER BY id`, org)
		var out []string
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			out = append(out, a)
		}
		return out
	}
	same := func(what string, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %q, want %q", what, got, want)
		}
	}

	// a project with a board: To do, In progress, Testing (promotes), Dev complete (to production), Done
	var pid, bid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ($1, 'p', 'FL') RETURNING id`, org).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, $2, 'Web') RETURNING id`, pid, org).Scan(&bid)
	col := map[string]string{}
	for i, name := range []string{"To do", "In progress", "Testing", "Dev complete", "Done"} {
		var id string
		_ = db.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, $2, $3) RETURNING id`, bid, name, i).Scan(&id)
		col[name] = id
	}
	_, _ = db.Exec(ctx, `UPDATE boards SET dev_complete_column_id = $2 WHERE id = $1`, bid, col["Dev complete"])
	needs := func(title string) *tickets.Ticket {
		t.Helper()
		tk, err := ts.CreateBacklog(ctx, org, pid, "feature", title, "")
		if err != nil {
			t.Fatal(err)
		}
		on := true
		if err := ts.SetFlag(ctx, org, tk.ID, tickets.FlagInput{Needs: &on}); err != nil {
			t.Fatal(err)
		}
		_, _ = db.Exec(ctx, `UPDATE tickets SET assigned_to = 'dev1' WHERE id = $1`, tk.ID)
		return tk
	}
	move := func(id, column string) {
		t.Helper()
		if err := ts.Move(ctx, org, id, col[column], 0); err != nil {
			t.Fatal(err)
		}
	}

	// not connected: moving a ticket that needs a flag queues nothing
	early := needs("before connecting")
	move(early.ID, "To do")
	same("not connected", jobs(), nil)

	// connecting: admins only, the token checked with flags.gg first and stored sealed
	body := `{"token":"flg_good","base_url":"` + srv.URL + `"}`
	if rec := call(fl.Connect, session("member"), "PUT", body, nil); rec.Code != 403 {
		t.Fatalf("member connect: %d", rec.Code)
	}
	if rec := call(fl.Connect, session("admin"), "PUT", `{"token":"flg_bad","base_url":"`+srv.URL+`"}`, nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad_flags_token") {
		t.Fatalf("bad token: %d %s", rec.Code, rec.Body)
	}
	if rec := call(fl.Connect, session("admin"), "PUT", body, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "agent-web") {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	var sealed []byte
	_ = db.QueryRow(ctx, `SELECT secret FROM org_integrations WHERE org_id = $1 AND provider = 'flags'`, org).Scan(&sealed)
	if bytes.Contains(sealed, []byte("flg_good")) {
		t.Fatal("token stored in the clear")
	}
	if rec := call(fl.Connection, session("member"), "GET", "", nil); rec.Code != 200 || strings.Contains(rec.Body.String(), "flg_good") || !strings.Contains(rec.Body.String(), `"agent_id":"agent-web"`) {
		t.Fatalf("connection: %d %s", rec.Code, rec.Body)
	}

	// no agent for the project yet: the create fails, shown on the ticket
	first := needs("Keep the basket")
	move(first.ID, "To do")
	same("onto a board", jobs(), []string{"create"})
	tick()
	if got, _ := ts.Get(ctx, org, first.ID); got.FlagError == nil || !strings.Contains(*got.FlagError, "agent") {
		t.Fatalf("no agent: %+v", got.FlagError)
	}
	same("dropped", jobs(), nil)

	// the project's agent: one the token sees; cleanup days 1–365
	p := map[string]string{"id": pid}
	if rec := call(fl.SetProjectSettings, session("admin"), "PUT", `{"agent_id":"nope"}`, p); rec.Code != 400 {
		t.Fatalf("unknown agent: %d", rec.Code)
	}
	if rec := call(fl.SetProjectSettings, session("admin"), "PUT", `{"cleanup_days":0}`, p); rec.Code != 400 {
		t.Fatalf("bad days: %d", rec.Code)
	}
	if rec := call(fl.SetProjectSettings, session("member"), "PUT", `{"agent_id":"agent-web"}`, p); rec.Code != 403 {
		t.Fatalf("member: %d", rec.Code)
	}
	if rec := call(fl.SetProjectSettings, session("admin"), "PUT", `{"agent_id":"agent-web","cleanup_days":14}`, p); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cleanup_days":14`) {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body)
	}

	// onto a board: created off in dev, named after the key; the name is then locked
	tk := needs("Gift cards")
	fake.took()
	move(tk.ID, "To do")
	tick()
	g, _ := ts.Get(ctx, org, tk.ID)
	key := g.Key
	same("create", fake.took(), []string{"PUT /integration/tickets/" + key + "/flag"})
	if f := fake.flags[key]; f == nil || f.Name != strings.ToLower(key) || f.AgentID != "agent-web" {
		t.Fatalf("in flags.gg: %+v", f)
	}
	if got, _ := ts.Get(ctx, org, tk.ID); got.FlagCreatedAt == nil || got.FlagError != nil || got.FlagName == nil || *got.FlagName != strings.ToLower(key) {
		t.Fatalf("ticket after create: %+v", got.Ticket)
	}
	renamed := "gift-cards"
	if err := ts.SetFlag(ctx, org, tk.ID, tickets.FlagInput{Name: &renamed}); !errors.Is(err, tickets.ErrFlagNameLocked) {
		t.Fatalf("rename after create: %v", err)
	}
	bad := "no/slashes"
	if err := ts.SetFlag(ctx, org, first.ID, tickets.FlagInput{Name: &bad}); !errors.Is(err, tickets.ErrBadFlagName) {
		t.Fatalf("bad name: %v", err)
	}

	// a board's own agent beats the project's (TL-804: frontend agent = Frontend board); "" goes back to it
	b := map[string]string{"id": bid}
	if rec := call(fl.SetBoardAgent, session("member"), "PUT", `{"agent_id":"agent-apps"}`, b); rec.Code != 403 {
		t.Fatalf("member board agent: %d", rec.Code)
	}
	if rec := call(fl.SetBoardAgent, session("admin"), "PUT", `{"agent_id":"nope"}`, b); rec.Code != 400 {
		t.Fatalf("unknown board agent: %d", rec.Code)
	}
	if rec := call(fl.SetBoardAgent, session("admin"), "PUT", `{}`, b); rec.Code != 400 {
		t.Fatalf("no board agent: %d", rec.Code)
	}
	if rec := call(fl.SetBoardAgent, session("admin"), "PUT", `{"agent_id":"agent-apps"}`, b); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"agent_id":"agent-apps"`) {
		t.Fatalf("board agent: %d %s", rec.Code, rec.Body)
	}
	apps := needs("Apps only")
	move(apps.ID, "To do")
	tick()
	if g, _ := ts.Get(ctx, org, apps.ID); fake.flags[g.Key] == nil || fake.flags[g.Key].AgentID != "agent-apps" {
		t.Fatalf("board's agent not used: %+v", fake.flags[g.Key])
	}
	if rec := call(fl.SetBoardAgent, session("admin"), "PUT", `{"agent_id":""}`, b); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"agent_id":null`) {
		t.Fatalf("board agent cleared: %d %s", rec.Code, rec.Body)
	}
	fake.took()

	// the panel: read and toggled through us; the token never leaves the server
	tid := map[string]string{"id": tk.ID}
	if rec := call(fl.SetTicketFlag, session("member"), "PATCH", `{"environment_id":"dev","enabled":true}`, tid); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("toggle: %d %s", rec.Code, rec.Body)
	}
	if rec := call(fl.SetTicketFlag, session("member"), "PATCH", `{"environment_id":"prod","enabled":true}`, tid); rec.Code != 404 {
		t.Fatalf("toggle where it isn't yet: %d %s", rec.Code, rec.Body)
	}
	// specialised (TL-821): rollout and rules go through as sent, only the fields given
	fake.took()
	if rec := call(fl.SetTicketFlag, session("member"), "PATCH", `{"environment_id":"dev","rollout":25,"rules":[{"attribute":"key","operator":"in","values":["u_8812"],"serve":true}]}`, tid); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"rollout":25`) || !strings.Contains(rec.Body.String(), `"values":["u_8812"]`) || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("specialise: %d %s", rec.Code, rec.Body)
	}
	if rec := call(fl.SetTicketFlag, session("member"), "PATCH", `{"environment_id":"dev","rollout":100,"rules":[]}`, tid); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rollout":100,"rules":[]`) {
		t.Fatalf("make generic: %d %s", rec.Code, rec.Body)
	}
	if got := fake.took(); len(got) != 2 {
		t.Fatalf("specialise calls: %v", got)
	}
	for _, body := range []string{
		`{"environment_id":"dev"}`,
		`{"rollout":10}`,
		`{"environment_id":"dev","rollout":101}`,
		`{"environment_id":"dev","rollout":-1}`,
		`{"environment_id":"dev","rules":[{"attribute":"key","operator":"regex","values":["x"],"serve":true}]}`,
		`{"environment_id":"dev","rules":[{"attribute":"","operator":"in","values":["x"],"serve":true}]}`,
		`{"environment_id":"dev","rules":[{"attribute":"key","operator":"in","values":[],"serve":true}]}`,
		`{"environment_id":"dev","rules":[{"attribute":"key","operator":"in","values":[""],"serve":true}]}`,
	} {
		if rec := call(fl.SetTicketFlag, session("member"), "PATCH", body, tid); rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad_flag_toggle") {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if got := fake.took(); len(got) != 0 {
		t.Fatalf("bad changes reached flags.gg: %v", got)
	}
	if rec := call(fl.SetTicketFlag, sessionIn("x", "org_other"), "PATCH", `{"environment_id":"dev","rollout":10}`, tid); rec.Code != 404 {
		t.Fatalf("another org specialises: %d", rec.Code)
	}
	if rec := call(fl.TicketFlag, session("member"), "GET", "", tid); rec.Code != 200 || strings.Contains(rec.Body.String(), "flg_good") {
		t.Fatalf("panel: %d %s", rec.Code, rec.Body)
	}
	if rec := call(fl.TicketFlag, sessionIn("x", "org_other"), "GET", "", tid); rec.Code != 404 {
		t.Fatalf("another org: %d", rec.Code)
	}
	fake.took()

	// In progress does nothing; Testing promotes; back to In progress does nothing
	move(tk.ID, "In progress")
	same("in progress", jobs(), nil)
	move(tk.ID, "Testing")
	move(tk.ID, "In progress")
	same("testing, then back", jobs(), []string{"promote"})
	tick()
	same("promote", fake.took(), []string{"POST /integration/tickets/" + key + "/flag/promote"})

	// flags.gg down: the move still happens; the job waits and is tried again later
	fake.down = true
	move(tk.ID, "Dev complete")
	tick()
	var attempts int
	_ = db.QueryRow(ctx, `SELECT attempts FROM flag_outbox WHERE org_id = $1`, org).Scan(&attempts)
	if attempts != 1 {
		t.Fatalf("retry: attempts %d", attempts)
	}
	fake.down = false
	_, _ = db.Exec(ctx, `UPDATE flag_outbox SET next_at = now() WHERE org_id = $1`, org)
	fake.took()
	tick()
	same("to production", fake.took(), []string{"POST /integration/tickets/" + key + "/flag/promote?to=production"})

	// in production: one "Remove flag" ticket, due in the project's 14 days, for whoever had it
	var cleanup string
	var due, assignee string
	if err := db.QueryRow(ctx, `SELECT c.id, to_char(c.due_date, 'YYYY-MM-DD'), c.assigned_to FROM tickets o JOIN tickets c ON c.id = o.flag_cleanup_id WHERE o.id = $1`, tk.ID).
		Scan(&cleanup, &due, &assignee); err != nil {
		t.Fatalf("cleanup ticket: %v", err)
	}
	if due != "2026-11-18" || assignee != "dev1" {
		t.Fatalf("cleanup due %s for %s", due, assignee)
	}
	if c, _ := ts.Get(ctx, org, cleanup); !strings.Contains(c.Title, strings.ToLower(key)) || len(c.Labels) != 1 || c.Labels[0] != "flag-cleanup" || c.ColumnID != nil {
		t.Fatalf("cleanup: %+v", c.Ticket)
	}
	move(tk.ID, "Testing")
	move(tk.ID, "Dev complete")
	tick()
	var cleanups int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE project_id = $1 AND title LIKE 'Remove flag%'`, pid).Scan(&cleanups)
	if cleanups != 1 {
		t.Fatalf("cleanup tickets: %d", cleanups)
	}
	fake.took()

	// the cleanup ticket done: the flag is archived; won't do would have left it
	if err := ts.ResolveAs(ctx, org, "dev1", cleanup, tickets.ResolutionDone, ""); err != nil {
		t.Fatal(err)
	}
	same("cleanup done", jobs(), []string{"archive"})
	tick()
	same("archive", fake.took(), []string{"DELETE /integration/tickets/" + key + "/flag"})
	if got, _ := ts.Get(ctx, org, tk.ID); got.FlagCreatedAt != nil {
		t.Fatal("archived: still marked made")
	}

	// ticked after it reached Testing: the promote finds no flag, so it's made and promoted
	late, _ := ts.CreateBacklog(ctx, org, pid, "feature", "Late", "")
	move(late.ID, "To do")
	same("not needed", jobs(), nil)
	on := true
	if err := ts.SetFlag(ctx, org, late.ID, tickets.FlagInput{Needs: &on}); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(ctx, `DELETE FROM flag_outbox WHERE org_id = $1`, org) // as if flags.gg had lost it
	move(late.ID, "Testing")
	tick()
	if c := fake.took(); len(c) != 3 || !strings.HasPrefix(c[0], "POST") || !strings.HasPrefix(c[1], "PUT") || !strings.HasPrefix(c[2], "POST") {
		t.Fatalf("promote before create: %q", c)
	}

	// a name clash: shown on the ticket for someone to rename
	fake.clash = true
	clash := needs("Clash")
	move(clash.ID, "To do")
	tick()
	if got, _ := ts.Get(ctx, org, clash.ID); got.FlagError == nil || !strings.Contains(*got.FlagError, "already exists") {
		t.Fatalf("clash: %+v", got.FlagError)
	}
	fake.clash = false

	// won't do, duplicate and delete archive the flag; Remove flag does too, and stops needing one
	if err := ts.Delete(ctx, org, late.ID); err != nil {
		t.Fatal(err)
	}
	same("deleted", jobs(), []string{"archive"})
	tick()
	if rec := call(fl.RemoveTicketFlag, session("member"), "DELETE", "", map[string]string{"id": first.ID}); rec.Code != 204 {
		t.Fatalf("remove: %d", rec.Code)
	}
	same("removed", jobs(), []string{"archive"})
	if got, _ := ts.Get(ctx, org, first.ID); got.NeedsFlag {
		t.Fatal("removed: still needs a flag")
	}
	tick()

	// disconnected: queued jobs are dropped without calling anyone
	if err := store.Disconnect(ctx, org, Flags); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(ctx, `INSERT INTO flag_outbox (org_id, ticket_id, action) VALUES ($1, $2, 'promote')`, org, tk.ID)
	fake.took()
	tick()
	same("disconnected", jobs(), nil)
	same("disconnected calls", fake.took(), nil)
}

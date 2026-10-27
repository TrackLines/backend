package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/retention"
)

var (
	once   sync.Once
	shared *pgxpool.Pool
)

func db(t *testing.T) *pgxpool.Pool {
	t.Helper()
	once.Do(func() {
		url := os.Getenv("TEST_DATABASE_URL")
		m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
		if err == nil {
			if err = m.Up(); errors.Is(err, migrate.ErrNoChange) {
				err = nil
			}
		}
		if err == nil {
			shared, err = pgxpool.New(context.Background(), url)
		}
		if err != nil {
			t.Fatal(err)
		}
	})
	return shared
}

type queued struct {
	ID, Org, ActorKind, Action string
	Actor, TargetID            *string
	Data                       map[string]any
}

func outbox(t *testing.T, org string) []queued {
	t.Helper()
	rows, err := db(t).Query(context.Background(), `SELECT id::text, org_id, actor_kind, action, actor, target_id, data FROM audit_outbox WHERE org_id = $1 ORDER BY queued`, org)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (queued, error) {
		var q queued
		return q, r.Scan(&q.ID, &q.Org, &q.ActorKind, &q.Action, &q.Actor, &q.TargetID, &q.Data)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// a project with a board of To do → Doing → Done, in its own org
func board(t *testing.T, org string) (project, boardID string, cols [3]string) {
	t.Helper()
	ctx := context.Background()
	p := db(t)
	for _, q := range []string{`DELETE FROM audit_outbox WHERE org_id = $1`, `DELETE FROM projects WHERE owner_clerk_id = $1`, `DELETE FROM org_billing WHERE org_id = $1`} {
		if _, err := p.Exec(ctx, q, org); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'P') RETURNING id`, org).Scan(&project))
	must(p.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, $2, 'Web') RETURNING id`, project, org).Scan(&boardID))
	for i, name := range []string{"To do", "Doing", "Done"} {
		must(p.QueryRow(ctx, `INSERT INTO columns (board_id, name, position) VALUES ($1, $2, $3) RETURNING id`, boardID, name, i).Scan(&cols[i]))
	}
	return
}

// every ticket stage is queued with its history row, in the same transaction, under an id the backfill
// can work out; who did it and what kind they are come from SetActor
func TestTicketStagesQueued(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	project, b, cols := board(t, "org_au1")
	var ticket string
	if err := p.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position, created_by) VALUES ($1, $2, $3, 't', 0, 'user_1') RETURNING id`,
		project, b, cols[1]).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	// moved by an agent: SetActor names it and its kind
	agent := auth.WithAPIKey(ctx, "user_1", "org_au1", "Claude", "ai")
	if err := pgx.BeginFunc(agent, p, func(tx pgx.Tx) error {
		if err := boards.SetActor(agent, tx, ""); err != nil {
			return err
		}
		_, err := tx.Exec(agent, `UPDATE tickets SET column_id = $2 WHERE id = $1`, ticket, cols[2])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := outbox(t, "org_au1")
	var steps []string
	for _, q := range got {
		steps = append(steps, fmt.Sprintf("%s %v/%v %s %s", q.Action, q.Data["stage"], q.Data["outcome"], q.ActorKind, *q.Actor))
		if *q.TargetID != ticket || q.Data["board_name"] == "" {
			t.Fatalf("event: %+v", q)
		}
	}
	want := "[ticket.stage backlog/skipped person user_1 ticket.stage column/skipped person user_1 ticket.stage column/entered person user_1 ticket.stage column/entered agent Claude]"
	if fmt.Sprint(steps) != want {
		t.Fatalf("stages:\n got %v\nwant %s", steps, want)
	}
	if got[3].Data["done"] != true || got[3].Data["column_name"] != "Done" || got[0].Data["done"] != false {
		t.Fatalf("data: %+v", got[3].Data)
	}
	// each id is md5('ticket_history:' || its history row id)
	var mismatched int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM ticket_history h WHERE h.ticket_id = $1
		AND NOT EXISTS (SELECT 1 FROM audit_outbox o WHERE o.id = md5('ticket_history:' || h.id)::uuid)`, ticket).Scan(&mismatched); err != nil || mismatched != 0 {
		t.Fatalf("%d history rows without their event (%v)", mismatched, err)
	}
	// a change rolled back leaves nothing queued
	_ = pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, `UPDATE tickets SET column_id = $2 WHERE id = $1`, ticket, cols[0])
		return errors.New("roll back")
	})
	if n := len(outbox(t, "org_au1")); n != 4 {
		t.Fatalf("rolled back change queued: %d", n)
	}
}

// kinds of caller, as the auditor names them
func TestActorKind(t *testing.T) {
	ctx := context.Background()
	for want, c := range map[string]context.Context{
		"system":  ctx,
		"person":  auth.WithAPIKey(ctx, "user_1", "o", "", "personal"),
		"agent":   auth.WithAPIKey(ctx, "user_1", "o", "Claude", "ai"),
		"service": auth.WithAPIKey(ctx, "user_1", "o", "CI", "service"),
	} {
		if got := boards.ActorKind(c); got != want {
			t.Errorf("%s: got %s", want, got)
		}
	}
}

// purges and plan changes are queued too; a purged org's waiting events go, leaving only org.purged
func TestPurgeAndPlanEvents(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	project, _, _ := board(t, "org_au2")
	var ticket string
	if err := p.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position, created_by) VALUES ($1, 't', 0, 'user_1') RETURNING id`, project).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `DELETE FROM tickets_all WHERE id = $1`, ticket); err != nil {
		t.Fatal(err)
	}
	// plans: free → pro → enterprise → free; a change that isn't one queues nothing
	for _, q := range []string{
		`INSERT INTO org_billing (org_id) VALUES ('org_au2')`,
		`UPDATE org_billing SET stripe_status = 'active', plan = 'pro' WHERE org_id = 'org_au2'`,
		`UPDATE org_billing SET billing_email = 'a@b.c' WHERE org_id = 'org_au2'`,
		`UPDATE org_billing SET plan = 'enterprise' WHERE org_id = 'org_au2'`,
		`UPDATE org_billing SET stripe_status = 'canceled' WHERE org_id = 'org_au2'`,
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var got []string
	for _, q := range outbox(t, "org_au2") {
		if q.Action == "ticket.stage" {
			continue
		}
		got = append(got, fmt.Sprintf("%s %s %v→%v", q.Action, q.ActorKind, q.Data["from"], q.Data["plan"]))
		if q.Action == "ticket.purged" && *q.TargetID != ticket {
			t.Fatalf("purged: %+v", q)
		}
	}
	want := "[ticket.purged system <nil>→<nil> org.plan_changed system free→pro org.plan_changed system pro→enterprise org.plan_changed system enterprise→free]"
	if fmt.Sprint(got) != want {
		t.Fatalf("events:\n got %v\nwant %s", got, want)
	}

	// the org purge: everything waiting for it goes, and org.purged is what's sent
	if _, err := p.Exec(ctx, `INSERT INTO deleted_orgs (org_id, deleted_at) VALUES ('org_au2', now() - interval '31 days')`); err != nil {
		t.Fatal(err)
	}
	if n, err := retention.PurgeOrgs(ctx, p, time.Now()); err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if left := outbox(t, "org_au2"); len(left) != 1 || left[0].Action != "org.purged" || *left[0].TargetID != "org_au2" {
		t.Fatalf("after the purge: %+v", left)
	}
}

// Record queues an event by whoever's asking
func TestRecord(t *testing.T) {
	p := db(t)
	_, _ = p.Exec(context.Background(), `DELETE FROM audit_outbox WHERE org_id = 'org_au3'`)
	ctx := auth.WithAPIKey(context.Background(), "user_7", "org_au3", "", "personal")
	if err := Record(ctx, p, Event{Org: "org_au3", Action: "api_key.created", TargetType: "api_key", TargetID: "k1", Data: map[string]any{"kind": "ai"}}); err != nil {
		t.Fatal(err)
	}
	got := outbox(t, "org_au3")
	if len(got) != 1 || got[0].Action != "api_key.created" || *got[0].Actor != "user_7" || got[0].ActorKind != "person" || got[0].Data["kind"] != "ai" {
		t.Fatalf("recorded: %+v", got)
	}
}

// the relay: sends due events oldest first, deletes what the auditor accepted or refused, backs off when
// it can't be reached, sends nothing with the flag off, and two relays never send the same event
func TestRelay(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	if _, err := p.Exec(ctx, `DELETE FROM audit_outbox`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	var down, refuse atomic.Bool
	var calls atomic.Int32
	auditor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/internal/events" || r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Events []struct {
				ID     string `json:"id"`
				Action string `json:"action"`
			} `json:"events"`
		}
		_ = json.Unmarshal(body, &in)
		res := map[string]any{"accepted": []string{}, "rejected": []any{}}
		mu.Lock()
		for _, e := range in.Events {
			sent = append(sent, e.Action)
			if refuse.Load() && e.Action == "bad.event" {
				res["rejected"] = append(res["rejected"].([]any), map[string]string{"id": e.ID, "reason": "nope"})
			} else {
				res["accepted"] = append(res["accepted"].([]string), e.ID)
			}
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer auditor.Close()
	queue := func(actions ...string) {
		for _, a := range actions {
			if err := Record(ctx, p, Event{Org: "org_relay", Action: a}); err != nil {
				t.Fatal(err)
			}
		}
	}
	waiting := func() int {
		var n int
		_ = p.QueryRow(ctx, `SELECT count(*) FROM audit_outbox`).Scan(&n)
		return n
	}
	r := Relay{DB: p, URL: auditor.URL, Secret: "s3cret"}

	queue("a.one", "a.two", "bad.event")
	refuse.Store(true)
	if n, err := r.Tick(ctx); err != nil || n != 3 {
		t.Fatalf("tick: %d %v", n, err)
	}
	if fmt.Sprint(sent) != "[a.one a.two bad.event]" || waiting() != 0 {
		t.Fatalf("sent %v, %d waiting", sent, waiting())
	}

	// unreachable: kept, and not due again for a while
	down.Store(true)
	queue("b.one")
	if _, err := r.Tick(ctx); err == nil || waiting() != 1 {
		t.Fatalf("down: %v, %d waiting", err, waiting())
	}
	var later bool
	_ = p.QueryRow(ctx, `SELECT next_attempt_at > now() + interval '30 seconds' FROM audit_outbox`).Scan(&later)
	if !later {
		t.Fatal("no back-off")
	}
	if n, _ := r.Tick(ctx); n != 0 {
		t.Fatal("resent before its back-off")
	}
	down.Store(false)
	_, _ = p.Exec(ctx, `UPDATE audit_outbox SET next_attempt_at = now()`)
	if _, err := r.Tick(ctx); err != nil || waiting() != 0 {
		t.Fatalf("back up: %v, %d waiting", err, waiting())
	}

	// the flag off: Run sends nothing
	queue("c.one")
	before := calls.Load()
	off, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	Relay{DB: p, URL: auditor.URL, Secret: "s3cret", On: func() bool { return false }}.Run(off, 50*time.Millisecond)
	if calls.Load() != before || waiting() != 1 {
		t.Fatalf("sent with the flag off")
	}
	// no URL or secret: Run returns at once
	Relay{DB: p, On: func() bool { return true }}.Run(ctx, time.Millisecond)

	// two relays at once never send the same event
	_, _ = p.Exec(ctx, `DELETE FROM audit_outbox`)
	mu.Lock()
	sent = nil
	mu.Unlock()
	for i := range 40 {
		queue(fmt.Sprintf("d.n%d", i))
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				if n, err := r.Tick(ctx); err != nil || n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	seen := map[string]int{}
	for _, a := range sent {
		seen[a]++
	}
	if len(sent) != 40 || len(seen) != 40 || waiting() != 0 {
		t.Fatalf("sent %d (%d distinct), %d waiting", len(sent), len(seen), waiting())
	}
}

// TL-667: the backfill queues history written before the outbox under the same ids the trigger uses, and
// running it again queues nothing twice
func TestBackfill(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	project, b, cols := board(t, "org_bf")
	var ticket string
	if err := p.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, title, position, created_by) VALUES ($1, $2, $3, 't', 0, 'user_1') RETURNING id`,
		project, b, cols[2]).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	queuedNow := len(outbox(t, "org_bf"))
	// as if written before the outbox existed (or already delivered)
	if _, err := p.Exec(ctx, `DELETE FROM audit_outbox WHERE org_id = 'org_bf'`); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile("../migrations/000097_audit_backfill.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	got := outbox(t, "org_bf")
	if len(got) != queuedNow || queuedNow != 4 {
		t.Fatalf("backfilled %d, the trigger queued %d", len(got), queuedNow)
	}
	var mismatched int
	_ = p.QueryRow(ctx, `SELECT count(*) FROM ticket_history h WHERE h.ticket_id = $1
		AND NOT EXISTS (SELECT 1 FROM audit_outbox o WHERE o.id = audit_stage_id(h.id) AND o.data->>'stage' = h.stage)`, ticket).Scan(&mismatched)
	if mismatched != 0 || got[0].ActorKind != "person" || *got[0].Actor != "user_1" {
		t.Fatalf("%d rows without their event; first %+v", mismatched, got[0])
	}
}

// TL-679: the client reads a ticket's activity from the auditor, with the secret
func TestClientActivity(t *testing.T) {
	auditor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" || r.URL.Path != "/internal/orgs/org_1/tickets/t%2F1/activity" && r.URL.EscapedPath() != "/internal/orgs/org_1/tickets/t%2F1/activity" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"e1","at":"2026-10-27T09:00:00Z","actor":"user_1","actor_kind":"person","action":"ticket.assigned","data":{"to":"user_1"}}]`))
	}))
	defer auditor.Close()
	c := Client{URL: auditor.URL, Secret: "s3cret"}
	got, err := c.Activity(context.Background(), "org_1", "t/1")
	if err != nil || len(got) != 1 || got[0].Action != "ticket.assigned" || string(got[0].Data) != `{"to":"user_1"}` {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := (Client{URL: auditor.URL, Secret: "wrong"}).Activity(context.Background(), "org_1", "t/1"); err == nil {
		t.Fatal("a refusal read as success")
	}
}

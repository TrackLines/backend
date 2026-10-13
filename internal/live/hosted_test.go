package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/valkey-io/valkey-go"
)

func valkeyForTest(t *testing.T) valkey.Client {
	t.Helper()
	ctx := context.Background()
	container, err := tcvalkey.Run(ctx, "valkey/valkey:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := valkey.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	vk, err := valkey.NewClient(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vk.Close)
	return vk
}

// A hosted refinement through its handlers (TL-223): one session per project; the host starts,
// steers what's open and can hand over; people join with heartbeats; other orgs can't see it; a
// session whose host has gone for two minutes ends; late joiners get the current focus.
func TestHostedRefinement(t *testing.T) {
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
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_hs', 'Hosted') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	clock := time.UnixMilli(1_000_000)
	s := System{VK: valkeyForTest(t), Boards: boards.Store{DB: db}, Now: func() time.Time { return clock }}
	type call struct {
		code int
		h    Hosted
		body string
	}
	as := func(user, org string) context.Context { return auth.WithAPIKey(ctx, user, org, "ui", "") }
	send := func(user, org, route, body string) call {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(as(user, org))
		req.SetPathValue("id", pid)
		rec := httptest.NewRecorder()
		map[string]func(){"start": func() { s.RefinementStart(rec, req) }, "get": func() { s.RefinementGet(rec, req) }, "join": func() { s.RefinementJoin(rec, req) },
			"leave": func() { s.RefinementLeave(rec, req) }, "focus": func() { s.RefinementFocus(rec, req) }, "host": func() { s.RefinementHost(rec, req) },
			"end": func() { s.RefinementEnd(rec, req) }}[route]()
		var h Hosted
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		return call{rec.Code, h, rec.Body.String()}
	}
	expect := func(want int, user, org, route, body string) Hosted {
		t.Helper()
		c := send(user, org, route, body)
		if c.code != want {
			t.Fatalf("%s as %s/%s %s: %d %s, want %d", route, org, user, body, c.code, c.body, want)
		}
		return c.h
	}

	// nothing running; another org can't even see the project
	expect(404, "ava", "org_hs", "get", "")
	expect(404, "eve", "org_other", "start", "")

	// Ava starts and hosts; a second session is refused while hers runs
	h := expect(201, "ava", "org_hs", "start", "")
	if h.Host != "ava" || !slices.Equal(h.People, []string{"ava"}) {
		t.Fatalf("started: %+v", h)
	}
	expect(409, "bo", "org_hs", "start", "")

	// Bo joins; only the host can say what's open
	if h := expect(200, "bo", "org_hs", "join", ""); !slices.Contains(h.People, "bo") {
		t.Fatalf("joined: %+v", h)
	}
	expect(403, "bo", "org_hs", "focus", `{"ticketId":"t1"}`)
	h = expect(200, "ava", "org_hs", "focus", `{"ticketId":"t1","filters":{"labels":["ui"]}}`)
	if h.Focus.TicketID == nil || *h.Focus.TicketID != "t1" || h.Focus.Filters["labels"][0] != "ui" {
		t.Fatalf("focus: %+v", h.Focus)
	}

	// a late joiner gets what the host has open now
	if h := expect(200, "cy", "org_hs", "join", ""); h.Focus.TicketID == nil || *h.Focus.TicketID != "t1" {
		t.Fatalf("late joiner's focus: %+v", h.Focus)
	}
	var latest Event
	raw, _ := s.VK.Do(ctx, s.VK.B().Get().Key(projectScope(refinement, pid).events).Build()).ToString()
	if json.Unmarshal([]byte(raw), &latest) != nil || latest.Type != "refinement-session" {
		t.Fatalf("retained event for late stream connections: %s", raw)
	}

	// hand over: only the host, and only to someone who's here
	expect(403, "bo", "org_hs", "host", `{"user_id":"bo"}`)
	expect(400, "ava", "org_hs", "host", `{"user_id":"zed"}`)
	if h := expect(200, "ava", "org_hs", "host", `{"user_id":"bo"}`); h.Host != "bo" {
		t.Fatalf("handed over: %+v", h)
	}
	expect(403, "ava", "org_hs", "end", "") // no longer the host

	// people drop off after a minute without a heartbeat; the host gone 2 minutes ends it
	clock = clock.Add(61 * time.Second)
	expect(200, "bo", "org_hs", "join", "") // Bo, the host, is still here
	if h := expect(200, "bo", "org_hs", "get", ""); !slices.Equal(h.People, []string{"bo"}) {
		t.Fatalf("after a minute: %+v", h.People)
	}
	expect(200, "cy", "org_hs", "join", "")
	clock = clock.Add(90 * time.Second) // Bo gone 90s; Cy, still heartbeating, can take over
	expect(200, "cy", "org_hs", "join", "")
	if h := expect(200, "cy", "org_hs", "host", `{"user_id":"cy"}`); h.Host != "cy" {
		t.Fatalf("took over: %+v", h)
	}
	clock = clock.Add(3 * time.Minute) // now Cy is gone too, for over two minutes
	expect(404, "dee", "org_hs", "get", "")
	// a new session can start; leaving and ending work
	expect(201, "dee", "org_hs", "start", "")
	expect(200, "ava", "org_hs", "join", "")
	expect(204, "ava", "org_hs", "leave", "")
	if h := expect(200, "dee", "org_hs", "get", ""); slices.Contains(h.People, "ava") {
		t.Fatalf("left: %+v", h.People)
	}
	expect(204, "dee", "org_hs", "end", "")
	expect(404, "dee", "org_hs", "get", "")
}

// A hosted standup (TL-225): starting the standup makes the starter the host; others join and follow
// what the host has open; ending the standup ends the session; other orgs can't see it.
func TestHostedStandup(t *testing.T) {
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
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_hsu', 'Standups') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, err := boards.Store{DB: db}.Create(ctx, "org_hsu", pid, "Team", "")
	if err != nil {
		t.Fatal(err)
	}
	clock := time.UnixMilli(5_000_000)
	s := System{VK: valkeyForTest(t), Boards: boards.Store{DB: db}, Now: func() time.Time { return clock }}
	send := func(user, org, route, body string) (int, Hosted, string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(auth.WithAPIKey(ctx, user, org, "ui", ""))
		req.SetPathValue("id", b.ID)
		rec := httptest.NewRecorder()
		map[string]func(){"standup": func() { s.Start(rec, req) }, "act": func() { s.Act(rec, req) }, "session": func() { s.StandupSession(rec, req) },
			"join": func() { s.StandupJoin(rec, req) }, "leave": func() { s.StandupLeave(rec, req) }, "focus": func() { s.StandupFocus(rec, req) },
			"host": func() { s.StandupHost(rec, req) }}[route]()
		var h Hosted
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		return rec.Code, h, rec.Body.String()
	}
	expect := func(want int, user, org, route, body string) Hosted {
		t.Helper()
		code, h, raw := send(user, org, route, body)
		if code != want {
			t.Fatalf("%s as %s/%s %s: %d %s, want %d", route, org, user, body, code, raw, want)
		}
		return h
	}

	expect(404, "ava", "org_hsu", "session", "")
	expect(201, "ava", "org_hsu", "standup", `{"people":[{"id":"ava","label":"Ava"},{"id":"bo","label":"Bo"}]}`)
	if h := expect(200, "bo", "org_hsu", "session", ""); h.Host != "ava" {
		t.Fatalf("starter hosts: %+v", h)
	}
	// the clock waits for the host: nobody's turn has started yet (TL-231)
	if v, err := s.get(ctx, b.ID); err != nil || v.RunningSince != nil || v.SpentMS != 0 || v.Index != 0 {
		t.Fatalf("a new standup should wait to start: %+v %v", v, err)
	}
	expect(404, "eve", "org_other", "session", "")
	if h := expect(200, "bo", "org_hsu", "join", ""); !slices.Contains(h.People, "bo") {
		t.Fatalf("joined: %+v", h)
	}
	expect(403, "bo", "org_hsu", "focus", `{"ticketId":"t9"}`)
	h := expect(200, "ava", "org_hsu", "focus", `{"ticketId":"t9","filters":{"assignee":["mine"]}}`)
	if h.Focus.TicketID == nil || *h.Focus.TicketID != "t9" || h.Focus.Filters["assignee"][0] != "mine" {
		t.Fatalf("focus: %+v", h.Focus)
	}
	if h := expect(200, "ava", "org_hsu", "host", `{"user_id":"bo"}`); h.Host != "bo" {
		t.Fatalf("handed to the next speaker: %+v", h)
	}
	// its own retained event: the standup's isn't replaced
	if raw, _ := s.VK.Do(ctx, s.VK.B().Get().Key(eventsKey(b.ID)).Build()).ToString(); !strings.Contains(raw, `"type":"standup"`) {
		t.Fatalf("the standup's retained event was replaced: %s", raw)
	}
	// ending the standup ends the session; a new standup starts a fresh one
	expect(204, "bo", "org_hsu", "act", `{"action":"end"}`)
	expect(404, "bo", "org_hsu", "session", "")
	expect(201, "cy", "org_hsu", "standup", `{"people":[]}`)
	if h := expect(200, "cy", "org_hsu", "session", ""); h.Host != "cy" || h.Focus.TicketID != nil {
		t.Fatalf("fresh session: %+v", h)
	}
}

package tickets

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

func TestOpenTickets(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'ot1'; DELETE FROM projects WHERE owner_clerk_id = 'ot1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ot1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "ot1", pid, "Backend", "")
	s := Store{DB: db}
	mk := func(title, prio string, onBoard bool) string {
		var tk *Ticket
		if onBoard {
			tk, _ = s.Create(ctx, "ot1", b.Columns[0].ID, "task", title, "long description")
		} else {
			tk, _ = s.CreateBacklog(ctx, "ot1", pid, "task", title, "")
		}
		_ = s.Update(ctx, "ot1", tk.ID, "", title, "long description", prio)
		return tk.ID
	}
	backlogHigh := mk("backlog high", "high", false)
	boardHigh := mk("board high", "high", true)
	mk("backlog urgent", "urgent", false)
	medium := mk("board medium", "medium", true)
	done := mk("shipped", "urgent", true)
	_ = s.Move(ctx, "ot1", done, b.Columns[2].ID, 0)
	_ = s.SetBlockedBy(ctx, "ot1", medium, []string{backlogHigh})
	_, _ = s.Claim(ctx, "ot1", "claude", boardHigh)
	// only "board medium" is in the board's open sprint
	if _, err := db.Exec(ctx, `WITH sp AS (INSERT INTO sprints (board_id, number, length_days, ends_at) VALUES ($1, 1, 14, now() + interval '14 days') RETURNING id)
		UPDATE tickets SET sprint_id = (SELECT id FROM sp) WHERE id = $2`, b.ID, medium); err != nil {
		t.Fatal(err)
	}

	ids := func(ts []OpenTicket) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.Title)
		}
		return strings.Join(out, ", ")
	}
	all, err := s.OpenTickets(ctx, "ot1", pid, OpenFilter{})
	// open sprint first, then priority, then board before backlog, done left out
	if err != nil || ids(all) != "board medium, backlog urgent, board high, backlog high" {
		t.Fatalf("all: %s %v", ids(all), err)
	}
	if !all[0].InSprint || all[1].InSprint || all[2].InSprint {
		t.Fatalf("in_sprint: %+v", all)
	}
	if all[0].CreatedAt == "" || all[0].UpdatedAt == "" {
		t.Fatalf("open tickets need dates: %+v", all[0])
	}
	if all[2].BoardName == nil || *all[2].BoardName != "Backend" || *all[2].ColumnName != "To do" || all[1].BoardID != nil {
		t.Fatalf("where: %+v %+v", all[1], all[2])
	}
	if next, _ := s.OpenTickets(ctx, "ot1", pid, OpenFilter{Unassigned: true, ExcludeBlocked: true}); ids(next) != "backlog urgent, backlog high" {
		t.Fatalf("claimable: %s", ids(next))
	}
	if mine, _ := s.OpenTickets(ctx, "ot1", pid, OpenFilter{Assignee: "claude"}); ids(mine) != "board high" {
		t.Fatalf("mine: %s", ids(mine))
	}
	if _, err := s.OpenTickets(ctx, "other", pid, OpenFilter{}); !errors.Is(err, ErrNotFound) {
		t.Fatal("other org:", err)
	}

	// HTTP: no descriptions in the payload; me = the key's agent name; bad filter is 400
	h := NewSystem(db)
	get := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/?"+q, nil).WithContext(auth.WithAPIKeyUser(ctx, "ot1", "claude"))
		req.SetPathValue("id", pid)
		rec := httptest.NewRecorder()
		h.OpenTickets(rec, req)
		return rec
	}
	if rec := get("assignee=me"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "board high") || strings.Contains(rec.Body.String(), "long description") {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	if rec := get("assignee=someone"); rec.Code != 400 {
		t.Fatalf("bad assignee: %d", rec.Code)
	}
}

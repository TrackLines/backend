package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

func TestLastDay(t *testing.T) {
	auckland, err := time.LoadLocation("Pacific/Auckland") // UTC+13 in October
	if err != nil {
		t.Fatal(err)
	}
	ends := time.Date(2026, 10, 12, 0, 30, 0, 0, time.UTC) // 12 Oct 13:30 in Auckland
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)  // 12 Oct 01:00 in Auckland
	if lastDay(now, ends, time.UTC) {
		t.Error("UTC: 11 Oct isn't the last day of a sprint ending 12 Oct")
	}
	if !lastDay(now, ends, auckland) {
		t.Error("Auckland: it's already 12 Oct, the last day")
	}
	if !lastDay(ends.Add(time.Hour), ends, time.UTC) {
		t.Error("past the end (auto-close not run yet) counts as locked")
	}
	if lastDay(ends.Add(-48*time.Hour), ends, auckland) {
		t.Error("two days out isn't the last day")
	}
}

func TestWithZone(t *testing.T) {
	var got *time.Location
	h := httpx.WithZone(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = httpx.Zone(r.Context()) }))
	for header, want := range map[string]string{"Europe/London": "Europe/London", "": "UTC", "Not/AZone": "UTC", "Local": "UTC"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(httpx.ZoneHeader, header)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got.String() != want {
			t.Errorf("%q: zone %s, want %s", header, got, want)
		}
	}
}

func TestSprintLastDayLock(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'lk1'; DELETE FROM projects WHERE owner_clerk_id = 'lk1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('lk1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "lk1", pid, "b", "")
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID
	s := Store{DB: db}
	// sprint 1 runs for days yet; tickets join it normally
	if _, err := db.Exec(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at) VALUES ($1, 1, 14, now() + interval '3 days')`, b.ID); err != nil {
		t.Fatal(err)
	}
	inSprint, err := s.Create(ctx, "lk1", todo, "task", "planned", "")
	if err != nil || inSprint.ColumnID == nil {
		t.Fatalf("create before the last day: %+v %v", inSprint, err)
	}
	early, _ := s.CreateBacklog(ctx, "lk1", pid, "task", "early", "")
	if err := s.Move(ctx, "lk1", early.ID, todo, 0); err != nil {
		t.Fatalf("move in before the last day: %v", err)
	}

	// now it's the last day (ends_at passed, auto-close not run yet: same as "today" in any zone)
	if _, err := db.Exec(ctx, `UPDATE sprints SET ends_at = now() - interval '1 minute' WHERE board_id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	late, _ := s.CreateBacklog(ctx, "lk1", pid, "task", "late", "")
	var locked SprintLockedError
	if err := s.Move(ctx, "lk1", late.ID, todo, 0); !errors.As(err, &locked) || locked.Number != 1 {
		t.Fatalf("backlog → sprint on the last day: %v", err)
	}
	if got, _ := s.Get(ctx, "lk1", late.ID); got.ColumnID != nil {
		t.Fatal("refused move still placed the ticket")
	}
	// tickets already in the sprint still move, including to Done; and out to the backlog
	if err := s.Move(ctx, "lk1", inSprint.ID, doing, 0); err != nil {
		t.Fatalf("within the sprint: %v", err)
	}
	if err := s.Move(ctx, "lk1", inSprint.ID, done, 0); err != nil {
		t.Fatalf("to Done: %v", err)
	}
	if err := s.ToBacklog(ctx, "lk1", early.ID); err != nil {
		t.Fatalf("out of the sprint: %v", err)
	}
	// creating on the board goes to the bottom of the backlog instead
	created, err := s.Create(ctx, "lk1", todo, "bug", "found today", "details")
	if err != nil || created.ColumnID != nil || created.Title != "found today" || created.Type != "bug" {
		t.Fatalf("create on the last day: %+v %v", created, err)
	}
	if bl, _ := s.Backlog(ctx, "lk1", pid, ""); len(bl) == 0 || bl[len(bl)-1].ID != created.ID {
		t.Fatal("redirected ticket isn't at the bottom of the backlog")
	}

	// a finished sub-ticket still completes its on-board parent into Done, even one not in the
	// sprint: completion isn't new scope
	var onBoardParent string
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, type, title, position, created_by)
		VALUES ($1, $2, $3, 'task', 'parent on board', 99, 'lk1') RETURNING id`, pid, b.ID, todo).Scan(&onBoardParent)
	kid, _ := s.CreateBacklog(ctx, "lk1", pid, "task", "kid", "")
	if err := s.SetParent(ctx, "lk1", kid.ID, &onBoardParent); err != nil {
		t.Fatal(err)
	}
	if err := s.Resolve(ctx, "lk1", kid.ID); err != nil {
		t.Fatalf("resolve child: %v", err)
	}
	if got, _ := s.Get(ctx, "lk1", onBoardParent); !got.Done {
		t.Fatal("parent wasn't completed on the last day")
	}

	// HTTP: 409 with the message; create answers redirected_to_backlog and drops the estimate
	h := NewSystem(db)
	call := func(hf http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(auth.WithAPIKeyUser(ctx, "lk1", "claude"))
		req.SetPathValue("id", path)
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	if rec := call(h.Move, late.ID, `{"column_id":"`+todo+`"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "sprint 1 ends today") {
		t.Fatalf("http move: %d %s", rec.Code, rec.Body)
	}
	rec := call(h.Create, todo, `{"title":"via api","type":"task","estimate":"3"}`)
	var out struct {
		ColumnID            *string `json:"column_id"`
		Estimate            *string `json:"estimate"`
		RedirectedToBacklog bool    `json:"redirected_to_backlog"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.RedirectedToBacklog || out.ColumnID != nil || out.Estimate != nil {
		t.Fatalf("http create: %d %s", rec.Code, rec.Body)
	}
}

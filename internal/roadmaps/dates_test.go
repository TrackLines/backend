package roadmaps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

func str(s string) *string { return &s }

func TestValidDates(t *testing.T) {
	for _, c := range []struct {
		start, target *string
		ok            bool
	}{
		{nil, nil, true}, {str("2027-01-01"), nil, true}, {nil, str("2027-01-01"), true},
		{str("2027-01-01"), str("2027-02-01"), true}, {str("2027-02-01"), str("2027-02-01"), true},
		{str("2027-03-01"), str("2027-02-01"), false},
	} {
		if got := ValidDates(Item{StartDate: c.start, TargetDate: c.target}); got != c.ok {
			t.Errorf("%v→%v: got %v", c.start, c.target, got)
		}
	}
}

// PostgreSQL is provisioned by this package's TestMain.
func TestStartDates(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
	}
	TestItemTickets(t) // ensures migrations + user rl1
	ctx := context.Background()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('rl1', 'gantt') RETURNING id`).Scan(&pid)
	s := Store{DB: db}
	r, _ := s.Create(ctx, "rl1", pid, "Gantt", "", Public)

	if err := s.ReplaceItems(ctx, r.ID, "rl1", []Item{{Title: "Build", StartDate: str("2027-01-04"), TargetDate: str("2027-02-01")}, {Title: "Ship", TargetDate: str("2027-02-15")}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, r.ID)
	if *got.Items[0].StartDate != "2027-01-04" || *got.Items[0].TargetDate != "2027-02-01" || got.Items[1].StartDate != nil {
		t.Fatalf("dates: %+v %+v", got.Items[0], got.Items[1])
	}
	// resave by id keeps the start date
	if err := s.ReplaceItems(ctx, r.ID, "rl1", []Item{{ID: got.Items[0].ID, Title: "Build", StartDate: got.Items[0].StartDate, TargetDate: got.Items[0].TargetDate}}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx, r.ID); *got.Items[0].StartDate != "2027-01-04" {
		t.Fatalf("resave lost start date: %+v", got.Items[0])
	}
	// DB rule backs up the API check
	if err := s.ReplaceItems(ctx, r.ID, "rl1", []Item{{Title: "Bad", StartDate: str("2027-03-01"), TargetDate: str("2027-02-01")}}); !errors.Is(err, ErrBadDates) {
		t.Fatalf("start after target (store): %v", err)
	}
	// and the handler refuses it up front
	req := httptest.NewRequest("PUT", "/api/roadmaps/"+r.ID+"/items", strings.NewReader(`[{"title":"Bad","start_date":"2027-03-01","target_date":"2027-02-01"}]`)).
		WithContext(auth.WithAPIKeyUser(ctx, "rl1", "test-agent"))
	req.SetPathValue("id", r.ID)
	rec := httptest.NewRecorder()
	NewSystem(db).ReplaceItems(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "start date") {
		t.Fatalf("handler: %d %s", rec.Code, rec.Body)
	}
}

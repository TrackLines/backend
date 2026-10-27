package tickets_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/imports"
	"github.com/tracklines/backend/internal/tickets"
)

// The CSV export (TL-637): every ticket with its fields, spreadsheet formulas guarded, only the
// caller's org, and a file the CSV import reads back.
func TestExportCSV(t *testing.T) {
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
	const org = "org_export"
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ($1, 'Shop Front', 'SHP') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	s := tickets.Store{DB: db}
	a, _ := s.CreateBacklog(ctx, org, pid, "bug", "Login, \"again\"", "steps:\n1. open")
	b, _ := s.CreateBacklog(ctx, org, pid, "task", "=HYPERLINK(\"evil\")", "")
	if err := s.SetLabels(ctx, org, a.ID, []string{"ui", "Auth"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetDueDate(ctx, org, a.ID, "2026-11-02")
	if _, err := db.Exec(ctx, `UPDATE tickets SET resolved_at = now(), resolution = 'wont_do', assigned_to = 'user_x' WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}

	h := tickets.NewSystem(db)
	get := func(org string) (int, string, string) {
		req := httptest.NewRequest("GET", "/", nil).WithContext(auth.WithAPIKey(ctx, "user_e", org, "", "personal"))
		req.SetPathValue("id", pid)
		rec := httptest.NewRecorder()
		h.Export(rec, req)
		return rec.Code, rec.Header().Get("Content-Disposition"), rec.Body.String()
	}
	code, disposition, body := get(org)
	if code != 200 || !strings.Contains(disposition, `filename="shop-front-tickets.csv"`) {
		t.Fatalf("%d %q", code, disposition)
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || len(rows) != 3 || strings.Join(rows[0], ",") != strings.Join(tickets.ExportHeader, ",") {
		t.Fatalf("csv: %v %v", rows, err)
	}
	first := rows[1]
	if first[0] != a.Key || first[1] != `Login, "again"` || first[2] != "steps:\n1. open" || first[3] != "bug" || first[6] != "Auth; ui" || first[9] != "open" || first[14] != "2026-11-02" {
		t.Fatalf("row: %q", first)
	}
	second := rows[2]
	if second[1] != `'=HYPERLINK("evil")` || second[9] != "wont_do" || second[10] != "user_x" {
		t.Fatalf("guarded row: %q", second)
	}
	if code, _, _ := get("org_someone_else"); code != 404 {
		t.Fatalf("another org's project → %d", code)
	}

	// round trip: the CSV import reads the export back, mapping by the export's own headers
	raw, _ := json.Marshal(imports.CSVOptions{CSV: body, Name: "Copy", Prefix: "CPY", Mapping: map[string]string{
		"title": "Title", "description": "Description", "type": "Type", "priority": "Priority", "labels": "Labels", "status": "Column", "due": "Due", "created": "Created"}})
	p, err := imports.CSV{}.Plan(ctx, org, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Projects[0].Tickets
	if len(got) != 2 || got[0].Title != `Login, "again"` || got[0].Type != "bug" || got[0].DueDate == nil || *got[0].DueDate != "2026-11-02" ||
		strings.Join(got[0].Labels, "|") != "Auth|ui" || got[1].Title != `=HYPERLINK("evil")` || got[0].CreatedAt == nil {
		t.Fatalf("round trip: %+v", got)
	}
}

// TL-669: a paid org's audit log says who exported a project; a Free org's records nothing
func TestExportAudited(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ('org_ex_pro', 'active', 'enterprise') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	h := tickets.NewSystem(db)
	for _, org := range []string{"org_ex_pro", "org_ex_free"} {
		var pid string
		if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ($1, 'E', 'EX') RETURNING id`, org).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "/", nil).WithContext(auth.WithAPIKey(ctx, "user_e", org, "", "personal"))
		req.SetPathValue("id", pid)
		h.Export(httptest.NewRecorder(), req)
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE org_id = $1 AND action = 'export.downloaded' AND target_id = $2 AND data->>'kind' = 'csv'`, org, pid).Scan(&n)
		if want := map[string]int{"org_ex_pro": 1, "org_ex_free": 0}[org]; n != want {
			t.Fatalf("%s: %d export events", org, n)
		}
	}
}

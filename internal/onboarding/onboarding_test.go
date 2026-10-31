package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/projects"
	"github.com/tracklines/backend/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

func fixture(t *testing.T) (System, string) {
	t.Helper()
	db, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Fatalf("close migrations: %v %v", srcErr, dbErr)
	}
	org := "org_onboarding_" + uuid.NewString()
	_, err = db.Exec(context.Background(), `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin', 'test'), ($1, 'admin2', 'test')`, org)
	if err != nil {
		t.Fatal(err)
	}
	return System{DB: db, Admins: organizations.Admins{DB: db}}, org
}

func call(t *testing.T, h http.HandlerFunc, ctx context.Context, body string, want int) State {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/onboarding", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != want {
		t.Fatalf("got %d: %s; want %d", w.Code, w.Body, want)
	}
	var s State
	if want == 200 && json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatal(w.Body.String())
	}
	return s
}

func session(org, user string) context.Context {
	return clerk.ContextWithSessionClaims(context.Background(), &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
}

func TestSetupPermissionsResumeAndGuideVersions(t *testing.T) {
	h, org := fixture(t)
	ctx := session(org, "admin")
	if s := call(t, h.Status, session(org, "member"), "", 200); s.Admin || !s.Guide.Available || s.Step != "complete" || slices.Contains(s.Chapters, "github") || slices.Contains(s.Chapters, "project") {
		t.Fatalf("member guide permissions: %+v", s)
	}
	if s := call(t, h.CompleteGuide, session(org, "member"), `{"id":"getting-started","version":1}`, 200); s.Guide.Available {
		t.Fatal("member cannot finish independently of admin setup")
	}
	if s := call(t, h.ResetGuide, session(org, "member"), fmt.Sprintf(`{"org_id":%q}`, org), 200); !s.Guide.Available {
		t.Fatal("member cannot reset independently of admin setup")
	}
	var setupRows int
	if err := h.DB.QueryRow(context.Background(), `SELECT count(*) FROM org_onboarding WHERE org_id=$1`, org).Scan(&setupRows); err != nil || setupRows != 0 {
		t.Fatalf("member status changed org setup: %d %v", setupRows, err)
	}
	for _, f := range []http.HandlerFunc{h.Status, h.Create, h.Finish, h.CompleteGuide, h.ResetGuide} {
		call(t, f, auth.WithAPIKey(context.Background(), "admin", org, "agent", "ai"), `{}`, 403)
	}
	call(t, h.Create, session(org, "member"), `{}`, 403)
	s := call(t, h.Status, ctx, "", 200)
	if s.Step != "project" || !s.Admin {
		t.Fatalf("new: %+v", s)
	}
	call(t, h.CompleteGuide, ctx, `{"id":"getting-started","version":1}`, 400)
	call(t, h.Create, ctx, `{"name":"Website","key_prefix":"!"}`, 400)
	call(t, h.Create, ctx, `{"name":"Website","key_prefix":"WEB","template_id":"not-a-template"}`, 400)
	s = call(t, h.Create, ctx, `{"name":"Website","key_prefix":"web","template_id":"builtin:scrum"}`, 200)
	if s.Step != "invite" || s.ProjectID == nil || s.BoardID == nil {
		t.Fatalf("created: %+v", s)
	}
	var columns int
	if err := h.DB.QueryRow(context.Background(), `SELECT count(*) FROM columns WHERE board_id = $1`, s.BoardID).Scan(&columns); err != nil || columns != 4 {
		t.Fatalf("template columns: %d %v", columns, err)
	}
	// Another admin/device resumes the same organization; retries cannot create a spare project.
	retry := call(t, h.Create, session(org, "admin2"), `{"name":"Duplicate","key_prefix":"DUP"}`, 200)
	if *retry.BoardID != *s.BoardID {
		t.Fatal("retry changed board")
	}
	s = call(t, h.Status, ctx, "", 200)
	if s.Step != "invite" {
		t.Fatalf("resume: %+v", s)
	}
	s = call(t, h.Finish, ctx, "", 200) // invitations are optional
	if s.Step != "complete" || s.CompletedAt == nil || !s.Guide.Available {
		t.Fatalf("finish: %+v", s)
	}
	call(t, h.CompleteGuide, ctx, `{"id":"getting-started","version":999}`, 409)
	for range 2 {
		s = call(t, h.CompleteGuide, ctx, `{"id":"getting-started","version":1}`, 200)
	}
	if s.Guide.Available {
		t.Fatal("completed guide still offered")
	}
	if s = call(t, h.Status, session(org, "admin2"), "", 200); !s.Guide.Available {
		t.Fatal("one person hid another person's guide")
	}
	var count int
	_ = h.DB.QueryRow(context.Background(), `SELECT count(*) FROM guide_completions WHERE org_id=$1 AND user_id='admin'`, org).Scan(&count)
	if count != 1 {
		t.Fatalf("idempotency: %d", count)
	}
	// A completion for an older published version does not hide the current one.
	next, err := h.stateForGuide(context.Background(), org, "admin", Guide{ID: GuideID, Version: GuideVersion + 1})
	if err != nil || !next.Guide.Available || next.Step != "complete" {
		t.Fatalf("new guide without repeating setup: %+v %v", next, err)
	}
}

func TestMemberResetIsPersonalAndPreservesSetup(t *testing.T) {
	h, org := fixture(t)
	admin, member := session(org, "admin"), session(org, "member")
	call(t, h.Status, admin, "", 200)
	call(t, h.Create, admin, `{"name":"Team","key_prefix":"TEAM"}`, 200)
	setup := call(t, h.Finish, admin, "", 200)
	call(t, h.CompleteGuide, admin, `{"id":"getting-started","version":1}`, 200)
	for range 2 {
		call(t, h.CompleteGuide, member, `{"id":"getting-started","version":1}`, 200)
	}
	_, err := h.DB.Exec(context.Background(), `INSERT INTO guide_completions (org_id,user_id,guide_id,version) VALUES ($1,'member','getting-started',1), ($2,'member','other-guide',1)`, org+"_other", org)
	if err != nil {
		t.Fatal(err)
	}
	call(t, h.ResetGuide, member, fmt.Sprintf(`{"org_id":%q}`, org+"_other"), 403)
	if s := call(t, h.Status, member, "", 200); s.Guide.Available {
		t.Fatal("mismatched-org reset changed completion")
	}
	for range 2 {
		s := call(t, h.ResetGuide, member, fmt.Sprintf(`{"org_id":%q,"user_id":"admin"}`, org), 200)
		if s.Admin || !s.Guide.Available || s.Step != "complete" || slices.Contains(s.Chapters, "github") {
			t.Fatalf("reset member: %+v", s)
		}
	}
	if s := call(t, h.Status, admin, "", 200); s.Guide.Available || s.Step != setup.Step || s.ProjectID == nil || *s.ProjectID != *setup.ProjectID || s.BoardID == nil || *s.BoardID != *setup.BoardID || s.CompletedAt == nil || !s.CompletedAt.Equal(*setup.CompletedAt) {
		t.Fatalf("reset changed admin completion/setup: %+v", s)
	}
	var count int
	if err := h.DB.QueryRow(context.Background(), `SELECT count(*) FROM guide_completions WHERE org_id IN ($1,$2)`, org, org+"_other").Scan(&count); err != nil || count != 3 {
		t.Fatalf("reset touched other records: %d %v", count, err)
	}
	call(t, h.ResetGuide, context.Background(), fmt.Sprintf(`{"org_id":%q}`, org), 403)
	call(t, h.ResetGuide, member, `{}`, 403)
}

func TestGuideChaptersRespectRoleAndProjectCapacity(t *testing.T) {
	h, org := fixture(t)
	admin, member := session(org, "admin"), session(org, "member")
	s := call(t, h.Status, admin, "", 200)
	if !slices.Equal(s.Chapters, []string{"project", "ticket", "github", "tui", "mcp"}) {
		t.Fatalf("admin chapters: %v", s.Chapters)
	}
	// no projects yet, and a member can't create one (TL-735), so there's nothing to raise a ticket in either
	s = call(t, h.Status, member, "", 200)
	if !slices.Equal(s.Chapters, []string{"tui", "mcp"}) {
		t.Fatalf("member chapters: %v", s.Chapters)
	}
	call(t, h.Create, admin, `{"name":"Team","key_prefix":"TEAM"}`, 200)
	s = call(t, h.Status, member, "", 200)
	if !slices.Equal(s.Chapters, []string{"ticket", "tui", "mcp"}) {
		t.Fatalf("free plan full: %v", s.Chapters)
	}
	if _, err := h.DB.Exec(context.Background(), `INSERT INTO org_billing (org_id,stripe_status,plan) VALUES ($1,'active','pro')`, org); err != nil {
		t.Fatal(err)
	}
	s = call(t, h.Status, member, "", 200)
	if slices.Contains(s.Chapters, "project") || slices.Contains(s.Chapters, "github") {
		t.Fatalf("upgraded member: %v", s.Chapters) // room on the plan doesn't make them an admin
	}
	if s = call(t, h.Status, admin, "", 200); !slices.Contains(s.Chapters, "project") {
		t.Fatalf("upgraded admin: %v", s.Chapters)
	}
	call(t, h.Finish, member, "", 403)
}

func TestEmptyStartCreatesDefaultBoardWithoutTickets(t *testing.T) {
	h, org := fixture(t)
	ctx := session(org, "admin")
	call(t, h.Status, ctx, "", 200)
	s := call(t, h.Create, ctx, `{"name":"Empty","key_prefix":"EMPTY"}`, 200)
	var columns, tickets int
	if err := h.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM columns WHERE board_id=$1), (SELECT count(*) FROM tickets WHERE board_id=$1)`, s.BoardID).Scan(&columns, &tickets); err != nil || columns != 3 || tickets != 0 {
		t.Fatalf("empty board: %d columns, %d tickets, %v", columns, tickets, err)
	}
}

func TestImportAdoptionPlanLimitsAndIsolation(t *testing.T) {
	h, org := fixture(t)
	ctx := session(org, "admin")
	call(t, h.Status, ctx, "", 200) // initializes before leaving for import
	p, err := (projects.Store{DB: h.DB}).Create(context.Background(), org, "Imported", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := h.DB.QueryRow(context.Background(), `INSERT INTO boards (project_id,owner_clerk_id,name) VALUES ($1,$2,'Imported board') RETURNING id::text`, p.ID, org).Scan(&id); err != nil {
		t.Fatal(err)
	}
	call(t, h.Create, ctx, `{"name":"Spare","key_prefix":"SPARE"}`, 402)
	call(t, h.Create, ctx, `{"board_id":"missing"}`, 400)
	other := org + "_other"
	if err := h.initialize(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := h.setup(context.Background(), other, Setup{BoardID: id}, 1); !errors.Is(err, errBoard) {
		t.Fatalf("cross-org board: %v", err)
	}
	s := call(t, h.Create, ctx, fmt.Sprintf(`{"board_id":%q}`, id), 200)
	if s.ProjectID == nil || *s.ProjectID != p.ID {
		t.Fatalf("adopt: %+v", s)
	}
	call(t, h.Finish, ctx, "", 200)
	// Existing orgs get guides, but are not forced through project setup.
	established := org + "_established"
	if _, err := (projects.Store{DB: h.DB}).Create(context.Background(), established, "Existing", "", 1); err != nil {
		t.Fatal(err)
	}
	if err := h.initialize(context.Background(), established); err != nil {
		t.Fatal(err)
	}
	if s, err := h.state(context.Background(), established, "admin"); err != nil || s.Step != "complete" {
		t.Fatalf("existing org setup: %+v %v", s, err)
	}
	var total int
	_ = h.DB.QueryRow(context.Background(), `SELECT count(*) FROM projects WHERE owner_clerk_id=$1`, org).Scan(&total)
	if total != 1 {
		t.Fatalf("extra project: %d", total)
	}
}

func TestConcurrentSetupCreatesOneProjectAndBoard(t *testing.T) {
	h, org := fixture(t)
	if err := h.initialize(context.Background(), org); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.setup(context.Background(), org, Setup{Name: "Team", KeyPrefix: "TEAM", TemplateID: "builtin:kanban"}, 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var projects, boards int
	if err := h.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM projects WHERE owner_clerk_id=$1), (SELECT count(*) FROM boards WHERE owner_clerk_id=$1)`, org).Scan(&projects, &boards); err != nil || projects != 1 || boards != 1 {
		t.Fatalf("counts: %d %d %v", projects, boards, err)
	}
}

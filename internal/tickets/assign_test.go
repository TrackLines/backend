package tickets

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// PostgreSQL is provisioned by this package's TestMain.
func TestAssign(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
	}
	ctx := context.Background()
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('as1', 'as2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('as1', 'as2'); DELETE FROM boards WHERE owner_clerk_id IN ('as1', 'as2'); DELETE FROM projects WHERE owner_clerk_id IN ('as1', 'as2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('as1', 'a@b.c'), ('as2', 'd@e.f');
		INSERT INTO api_keys (owner_clerk_id, name, prefix, hash) VALUES
			('as1', 'codex', 'tl_a', 'h-as1-codex'), ('as1', 'gone', 'tl_b', 'h-as1-gone'), ('as2', 'stranger', 'tl_c', 'h-as2');
		INSERT INTO api_keys (owner_clerk_id, name, kind, prefix, hash) VALUES ('as1', 'deploy', 'service', 'tl_d', 'h-as1-deploy');
		UPDATE api_keys SET revoked_at = now() WHERE hash = 'h-as1-gone';
		UPDATE api_keys SET org_id = owner_clerk_id WHERE owner_clerk_id IN ('as1', 'as2')`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('as1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, title, position) VALUES ($1, 'x', 0) RETURNING id`, pid).Scan(&tid)
	s := Store{DB: db}
	str := func(v string) *string { return &v }

	if list, _ := s.Assignees(ctx, "as1", "as1"); len(list) != 2 || list[0].Label != "You" || list[0].Kind != "person" || list[1].ID != "codex" || list[1].Kind != "ai" {
		t.Fatalf("assignees: %+v", list)
	}
	for _, who := range []string{"as1", "codex"} {
		got, err := s.Assign(ctx, "as1", "as1", tid, str(who), false)
		if err != nil || *got.AssignedTo != who {
			t.Fatalf("assign %s: %v %+v", who, err, got)
		}
	}
	// owner override: reassign even though codex holds it
	if _, err := s.Assign(ctx, "as1", "as1", tid, str("as1"), false); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Assign(ctx, "as1", "as1", tid, str("as2"), true); err != nil || *got.AssignedTo != "as2" {
		t.Fatalf("assign organization member: %v %+v", err, got)
	}
	for _, bad := range []string{"gone", "deploy", "stranger", "as2", "anyone"} {
		if _, err := s.Assign(ctx, "as1", "as1", tid, str(bad), false); !errors.Is(err, ErrUnknownAssignee) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	if _, err := s.Assign(ctx, "as2", "as2", tid, str("as2"), false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user's ticket: %v", err)
	}
	if got, err := s.Assign(ctx, "as1", "as1", tid, nil, false); err != nil || got.AssignedTo != nil {
		t.Fatalf("unassign: %v %+v", err, got)
	}

	h := System{store: s, memberships: testAssigneeMembers{"user_1", "user_2"}}
	listReq := httptest.NewRequest("GET", "/api/assignees", nil)
	listReq = listReq.WithContext(auth.WithAPIKey(listReq.Context(), "user_1", "as1", "codex", "ai"))
	listRec := httptest.NewRecorder()
	h.Assignees(listRec, listReq)
	if listRec.Code != 200 || !strings.Contains(listRec.Body.String(), `"id":"user_2"`) || !strings.Contains(listRec.Body.String(), `"id":"codex"`) {
		t.Fatalf("assignee list: %d %s", listRec.Code, listRec.Body)
	}

	assignReq := httptest.NewRequest("PUT", "/api/tickets/"+tid+"/assignee", strings.NewReader(`{"assignee":"user_2"}`))
	assignReq.SetPathValue("id", tid)
	assignReq = assignReq.WithContext(auth.WithAPIKey(assignReq.Context(), "user_1", "as1", "codex", "ai"))
	assignRec := httptest.NewRecorder()
	h.Assign(assignRec, assignReq)
	if assignRec.Code != 200 || !strings.Contains(assignRec.Body.String(), `"assigned_to":"user_2"`) {
		t.Fatalf("assign org member: %d %s", assignRec.Code, assignRec.Body)
	}

	assignReq = httptest.NewRequest("PUT", "/api/tickets/"+tid+"/assignee", strings.NewReader(`{"assignee":"user_outside"}`))
	assignReq.SetPathValue("id", tid)
	assignReq = assignReq.WithContext(auth.WithAPIKey(assignReq.Context(), "user_1", "as1", "codex", "ai"))
	assignRec = httptest.NewRecorder()
	h.Assign(assignRec, assignReq)
	if assignRec.Code != 400 || !strings.Contains(assignRec.Body.String(), "active organization member") {
		t.Fatalf("assign non-member: %d %s", assignRec.Code, assignRec.Body)
	}
}

type testAssigneeMembers []string

func (m testAssigneeMembers) List(_ context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	list := &clerk.OrganizationMembershipList{}
	for _, id := range m {
		if len(params.UserIDs) != 0 && params.UserIDs[0] != id {
			continue
		}
		list.OrganizationMemberships = append(list.OrganizationMemberships, &clerk.OrganizationMembership{PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: id, Identifier: fmt.Sprintf("%s@example.test", id)}})
	}
	list.TotalCount = int64(len(list.OrganizationMemberships))
	return list, nil
}

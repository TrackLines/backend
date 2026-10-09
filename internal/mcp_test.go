package internal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tracklines/backend/internal/apikeys"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.key != "" {
		r.Header.Set("Authorization", "Bearer "+b.key)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestMCP drives /mcp with the SDK's own client, through the full API handler chain, as a tl_ key.
func TestMCP(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://migrations", "pgx5"+url[len("postgres"):])
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('mcp-user', 'm@c.p') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	key, _, err := apikeys.Store{DB: db}.Create(ctx, "mcp-user", "mcp-org", "claude", "ai")
	if err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('mcp-org', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "mcp-org", pid, "Backend", "")
	tk, _ := tickets.Store{DB: db}.Create(ctx, "mcp-org", b.Columns[0].ID, "bug", "Broken", "keep this description")

	srv := httptest.NewServer((&Service{DB: db}).Handler())
	defer srv.Close()
	connect := func(key string) (*sdk.ClientSession, error) {
		c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		return c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{key}}, MaxRetries: -1}, nil)
	}
	if _, err := connect(""); err == nil {
		t.Fatal("no key should be refused")
	}
	cs, err := connect(key)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) < 20 {
		t.Fatalf("tools: %v %v", len(list.Tools), err)
	}
	call := func(name string, args map[string]any) *sdk.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}
	text := func(res *sdk.CallToolResult) string { return res.Content[0].(*sdk.TextContent).Text }

	if res := call("get_board", map[string]any{"board_id": b.ID}); res.IsError || !strings.Contains(text(res), "Broken") {
		t.Fatalf("get_board: %s", text(res))
	}
	if res := call("claim_ticket", map[string]any{"ticket_id": tk.ID}); res.IsError || !strings.Contains(text(res), `"assigned_to":"claude"`) {
		t.Fatalf("claim as the key's agent: %s", text(res))
	}
	if res := call("list_open_tickets", map[string]any{"project_id": pid, "assignee": "me"}); res.IsError || !strings.Contains(text(res), tk.ID) {
		t.Fatalf("list_open_tickets mine: %s", text(res))
	}
	if res := call("list_open_tickets", map[string]any{"project_id": pid, "assignee": "unassigned", "blocked": "exclude"}); res.IsError || strings.Contains(text(res), tk.ID) {
		t.Fatalf("list_open_tickets claimable: %s", text(res))
	}
	// update with only a title keeps the description
	if res := call("update_ticket", map[string]any{"ticket_id": tk.ID, "title": "Fixed", "priority": "high"}); res.IsError {
		t.Fatalf("update: %s", text(res))
	}
	var title, desc, prio string
	_ = db.QueryRow(ctx, `SELECT title, description, priority::text FROM tickets WHERE id = $1`, tk.ID).Scan(&title, &desc, &prio)
	if title != "Fixed" || desc != "keep this description" || prio != "high" {
		t.Fatalf("after update: %q %q %q", title, desc, prio)
	}
	if res := call("add_comment", map[string]any{"ticket_id": tk.ID, "body": "done"}); res.IsError {
		t.Fatalf("comment: %s", text(res))
	}
	if res := call("move_ticket", map[string]any{"ticket_id": tk.ID, "column_id": b.Columns[2].ID, "position": 0}); res.IsError {
		t.Fatalf("move: %s", text(res))
	}
	if res := call("create_backlog_ticket", map[string]any{"project_id": pid, "title": "Later"}); !res.IsError {
		var later struct{ ID string }
		_ = json.Unmarshal([]byte(text(res)), &later)
		if res := call("resolve_ticket", map[string]any{"ticket_id": later.ID}); res.IsError {
			t.Fatalf("resolve_ticket: %s", text(res))
		}
	}
	if res := call("resolve_ticket", map[string]any{"ticket_id": tk.ID}); !res.IsError || !strings.Contains(text(res), "409") {
		t.Fatalf("resolving a board ticket should be refused: %s", text(res))
	}
	// errors come back as tool errors with the REST status, and org scoping holds
	if res := call("get_ticket", map[string]any{"ticket_id": "00000000-0000-0000-0000-000000000000"}); !res.IsError || !strings.Contains(text(res), "404") {
		t.Fatalf("missing ticket: %+v", res)
	}
	if res := call("get_ticket", map[string]any{}); !res.IsError || !strings.Contains(text(res), "ticket_id is required") {
		t.Fatalf("missing argument: %s", text(res))
	}
	var other string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('another-org', 'x') RETURNING id`).Scan(&other)
	if res := call("get_project", map[string]any{"project_id": other}); !res.IsError {
		t.Fatalf("another org's project should 404: %s", text(res))
	}
}

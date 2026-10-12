package integrations

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

func TestKeysIn(t *testing.T) {
	got := keysIn("TL", "tl-1-fix-login", "Fixes TL-2, not UTF-8 or TLM-3; TL-2 again", "")
	if !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("keys: %v", got)
	}
	if got := keysIn("TL", "no keys here"); len(got) != 0 {
		t.Fatalf("none: %v", got)
	}
}

func TestPRWebhooks(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'org_pr'; DELETE FROM projects WHERE owner_clerk_id = 'org_pr';
		DELETE FROM org_integrations WHERE org_id = 'org_pr'`); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	h := System{store: Store{DB: db, Box: box}, admins: organizations.Admins{DB: db}, tickets: tickets.Store{DB: db}, now: time.Now}

	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_pr', 'Shop', 'SHP') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "org_pr", pid, "web", "")
	ts := tickets.Store{DB: db}
	onBoard, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "Login form", "")
	inBacklog, _ := ts.CreateBacklog(ctx, "org_pr", pid, "bug", "Crash on save", "")
	// GitHub connected through installation 77; acme/shop and chewed/feed (Bitbucket) linked to the project
	if err := h.store.connect(ctx, "org_pr", GitHub, "https://api.github.com", "acme", "77", "77", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO project_repos (project_id, org_id, provider, full_name) VALUES ($1, 'org_pr', 'github', 'acme/shop'), ($1, 'org_pr', 'bitbucket', 'chewed/feed')`, pid); err != nil {
		t.Fatal(err)
	}

	sign := func(body, secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	github := func(body string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", sign(body, "gh"))
		rec := httptest.NewRecorder()
		h.GitHubWebhook("gh")(rec, req)
		return rec.Code
	}
	ghPR := func(state string, merged, draft bool, repo, installation string) string {
		m, d := "false", "false"
		if merged {
			m = "true"
		}
		if draft {
			d = "true"
		}
		return `{"action":"x","installation":{"id":` + installation + `},"repository":{"full_name":"` + repo + `"},
			"pull_request":{"number":5,"html_url":"https://github.com/acme/shop/pull/5","title":"Fix login (SHP-1)","body":"Also fixes shp-2.",
			"state":"` + state + `","draft":` + d + `,"merged":` + m + `,"user":{"login":"keloran"},"head":{"ref":"shp-1-login"}}}`
	}
	prs := func(ticket string) []LinkedPR {
		req := httptest.NewRequest("GET", "/", nil).WithContext(auth.WithAPIKeyUser(ctx, "org_pr", "claude"))
		req.SetPathValue("id", ticket)
		rec := httptest.NewRecorder()
		h.TicketPRs(rec, req)
		var out []LinkedPR
		_ = jsonUnmarshal(rec.Body.Bytes(), &out)
		return out
	}
	done := func(id string) bool {
		d, _ := ts.Get(ctx, "org_pr", id)
		return d.Done
	}
	comments := func(id string) int {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_comments WHERE ticket_id = $1 AND body LIKE 'Closed by acme/shop#5%'`, id).Scan(&n)
		return n
	}

	// opened as a draft: linked to both tickets it mentions (branch, title, body), nothing closes
	if code := github(ghPR("open", false, true, "acme/shop", "77")); code != 204 {
		t.Fatalf("opened: %d", code)
	}
	if p := prs(onBoard.ID); len(p) != 1 || p[0].State != "draft" || p[0].Number != 5 || p[0].Branch != "shp-1-login" {
		t.Fatalf("linked draft: %+v", p)
	}
	if len(prs(inBacklog.ID)) != 1 || done(onBoard.ID) {
		t.Fatal("backlog ticket not linked, or closed too early")
	}
	// ready, then merged: the board ticket goes to Done, the backlog one is resolved, with a comment
	_ = github(ghPR("open", false, false, "acme/shop", "77"))
	if p := prs(onBoard.ID); p[0].State != "open" {
		t.Fatalf("ready: %+v", p)
	}
	if code := github(ghPR("closed", true, false, "acme/shop", "77")); code != 204 {
		t.Fatalf("merged: %d", code)
	}
	if !done(onBoard.ID) || !done(inBacklog.ID) {
		t.Fatal("merge didn't close the tickets")
	}
	if d, _ := ts.Get(ctx, "org_pr", onBoard.ID); d.ColumnID == nil || *d.ColumnID != b.Columns[2].ID {
		t.Fatalf("board ticket not in Done: %+v", d)
	}
	_ = github(ghPR("closed", true, false, "acme/shop", "77")) // redelivery
	if comments(onBoard.ID) != 1 || comments(inBacklog.ID) != 1 {
		t.Fatalf("closing comments: %d %d", comments(onBoard.ID), comments(inBacklog.ID))
	}
	if p := prs(onBoard.ID); len(p) != 1 || p[0].State != "merged" {
		t.Fatalf("merged state: %+v", p)
	}
	// other installations and unlinked repos are ignored
	other, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "untouched", "")
	if code := github(strings.ReplaceAll(ghPR("open", false, false, "acme/shop", "99"), "shp-1", "shp-3")); code != 204 || len(prs(other.ID)) != 0 {
		t.Fatal("another installation linked a PR")
	}
	if code := github(strings.ReplaceAll(ghPR("open", false, false, "acme/elsewhere", "77"), "shp-1", "shp-3")); code != 204 || len(prs(other.ID)) != 0 {
		t.Fatal("an unlinked repo linked a PR")
	}

	// Bitbucket: pullrequest:fulfilled closes the ticket it mentions. Each linked repo has its own
	// webhook secret, sealed to the link (TL-165); deliveries name the link.
	var bbLink string
	_ = db.QueryRow(ctx, `SELECT id FROM project_repos WHERE org_id = 'org_pr' AND provider = 'bitbucket'`).Scan(&bbLink)
	sealedHook, _ := box.SealWithAAD("bb", "bitbucket-hook|"+bbLink)
	if _, err := db.Exec(ctx, `UPDATE project_repos SET webhook_id = '{h}', webhook_secret = $2 WHERE id = $1`, bbLink, sealedHook); err != nil {
		t.Fatal(err)
	}
	bbTicket, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "Feed parser", "")
	bb := func(event, state string) int {
		body := `{"repository":{"full_name":"chewed/feed"},"pullrequest":{"id":12,"title":"Parser fix","description":"closes ` + bbTicket.Key + `",
			"state":"` + state + `","author":{"display_name":"Keloran"},"source":{"branch":{"name":"feature/parser"}},
			"links":{"html":{"href":"https://bitbucket.org/chewed/feed/pull-requests/12"}}}}`
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.Header.Set("X-Event-Key", event)
		req.Header.Set("X-Hub-Signature", sign(body, "bb"))
		req.SetPathValue("link", bbLink)
		rec := httptest.NewRecorder()
		h.BitbucketWebhook(rec, req)
		return rec.Code
	}
	if code := bb("pullrequest:created", "OPEN"); code != 204 || len(prs(bbTicket.ID)) != 1 || done(bbTicket.ID) {
		t.Fatalf("bitbucket created: %d %+v", code, prs(bbTicket.ID))
	}
	if code := bb("pullrequest:fulfilled", "MERGED"); code != 204 || !done(bbTicket.ID) {
		t.Fatalf("bitbucket merged: %d", code)
	}
	if p := prs(bbTicket.ID); p[0].Provider != "bitbucket" || p[0].State != "merged" || p[0].Author != "Keloran" {
		t.Fatalf("bitbucket pr: %+v", p)
	}
}

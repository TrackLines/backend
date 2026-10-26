package integrations

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
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
	mentionedOnly, _ := ts.CreateBacklog(ctx, "org_pr", pid, "task", "Sign-up", "") // SHP-3: named in passing (TL-651)
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
			"pull_request":{"number":5,"html_url":"https://github.com/acme/shop/pull/5","title":"Fix login (SHP-1)","body":"Also fixes shp-2. Sign-up (SHP-3) builds on this.",
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
	if len(prs(mentionedOnly.ID)) != 1 || done(mentionedOnly.ID) {
		t.Fatal("a ticket the description only mentions wasn't linked, or was closed (TL-651)")
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
	// a captured "merged" delivery replayed later can't re-close a ticket someone reopened (TL-170);
	// the body is what's remembered, so changing unsigned headers like X-GitHub-Delivery doesn't help
	if err := ts.Move(ctx, "org_pr", onBoard.ID, b.Columns[0].ID, 0); err != nil || done(onBoard.ID) {
		t.Fatalf("reopen: %v", err)
	}
	// a merge on its own is already idempotent; the real replay re-sends "opened" then "merged"
	if code := github(ghPR("open", false, false, "acme/shop", "77")); code != 204 {
		t.Fatalf("replayed open: %d", code)
	}
	if code := github(ghPR("closed", true, false, "acme/shop", "77")); code != 204 || done(onBoard.ID) {
		t.Fatalf("replayed open-then-merge re-closed the ticket: %d", code)
	}
	if p := prs(onBoard.ID); len(p) != 1 || p[0].State != "merged" {
		t.Fatalf("a replay changed the PR's state: %+v", p)
	}
	// a delivery whose handling failed is forgotten, so the provider's retry still gets processed
	retry := []byte(`{"retry":1}`)
	if first, _ := h.firstDelivery(ctx, "github", retry); !first {
		t.Fatal("new delivery seen as a replay")
	}
	h.forgetDelivery(ctx, "github", retry)
	if first, _ := h.firstDelivery(ctx, "github", retry); !first {
		t.Fatal("a failed delivery's retry was refused")
	}
	if first, _ := h.firstDelivery(ctx, "bitbucket:another-link", retry); !first {
		t.Fatal("deliveries are remembered per provider and link")
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

	// Finish on release (TL-531): with a Dev complete column, a GitHub merge moves its ticket there;
	// a published release whose tag contains the merge commit moves it to Done, once all its merged
	// PRs have shipped. GitHub answers whether a tag contains a commit through compare.
	contains := map[string]map[string]bool{"v1": {"aaa": true, "bbb": true}, "v2": {"aaa": true, "bbb": true, "ccc": true}}
	compares := 0
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/app/installations/77/access_tokens":
			_, _ = w.Write([]byte(`{"token":"inst"}`))
		case strings.HasPrefix(r.URL.Path, "/repos/acme/shop/compare/") && r.Header.Get("Authorization") == "Bearer inst":
			compares++
			sha, tag, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/compare/"), "...")
			if sha == "gone" {
				http.NotFound(w, r)
				return
			}
			status := "diverged"
			if contains[tag][sha] {
				status = "ahead"
			}
			_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, 500)
		}
	}))
	defer gh.Close()
	h.git = Git{GitHubAppID: "1", GitHubPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		GitHubAPI: gh.URL}
	if err := (boards.Store{DB: db}).SetDevComplete(ctx, b.ID, "org_pr", b.Columns[1].ID); err != nil {
		t.Fatal(err)
	}
	send := func(event, body string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(body, "gh"))
		rec := httptest.NewRecorder()
		h.GitHubWebhook("gh")(rec, req)
		return rec.Code
	}
	merge := func(number, sha, ticketKey string) {
		t.Helper()
		body := `{"action":"closed","installation":{"id":77},"repository":{"full_name":"acme/shop"},"pull_request":{"number":` + number + `,
			"html_url":"https://github.com/acme/shop/pull/` + number + `","title":"` + ticketKey + ` work","body":"","state":"closed","draft":false,
			"merged":true,"merge_commit_sha":"` + sha + `","user":{"login":"keloran"},"head":{"ref":"x"}}}`
		if code := send("pull_request", body); code != 204 {
			t.Fatalf("merge #%s: %d", number, code)
		}
	}
	release := func(tag string, pre bool) int {
		p := "false"
		if pre {
			p = "true"
		}
		return send("release", `{"action":"published","installation":{"id":77},"repository":{"full_name":"acme/shop"},
			"release":{"tag_name":"`+tag+`","html_url":"https://github.com/acme/shop/releases/`+tag+`","prerelease":`+p+`}}`)
	}
	column := func(id string) string {
		d, _ := ts.Get(ctx, "org_pr", id)
		if d.ColumnID == nil {
			return ""
		}
		return *d.ColumnID
	}
	said := func(id, prefix string) int {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_comments WHERE ticket_id = $1 AND body LIKE $2`, id, prefix+"%").Scan(&n)
		return n
	}
	// release-done flag off: merging finishes as before, and releases are ignored
	flagOff, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "Flag off", "")
	merge("6", "fff", flagOff.Key)
	if !done(flagOff.ID) {
		t.Fatal("flag off: merge didn't finish the ticket")
	}
	if code := release("v0", false); code != 204 || compares != 0 {
		t.Fatalf("flag off: a release was used: %d %d", code, compares)
	}
	h = h.WithReleaseDone(func() bool { return true })
	// a ticket already in Dev complete (or past it) isn't moved by a merge, only told
	ahead, _ := ts.Create(ctx, "org_pr", b.Columns[1].ID, "task", "Ahead", "")
	merge("5", "eee", ahead.Key)
	if column(ahead.ID) != b.Columns[1].ID || said(ahead.ID, "Merged in acme/shop#5") != 1 {
		t.Fatalf("ahead: %s", column(ahead.ID))
	}
	one, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "One PR", "")
	two, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "Two PRs", "")
	lost, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "Force-pushed away", "")
	merge("7", "aaa", one.Key)
	merge("8", "bbb", two.Key)
	merge("9", "ccc", two.Key)
	merge("10", "gone", lost.Key)
	if column(one.ID) != b.Columns[1].ID || done(one.ID) || said(one.ID, "Merged in acme/shop#7") != 1 {
		t.Fatalf("merge didn't move to Dev complete: %s", column(one.ID))
	}
	if code := release("v2-rc", true); code != 204 || compares != 0 {
		t.Fatalf("a prerelease was used: %d %d", code, compares)
	}
	// v1 ships #7 and #8: "one" is done; "two" waits for #9; a sha GitHub doesn't know isn't shipped
	if code := release("v1", false); code != 204 {
		t.Fatalf("release v1: %d", code)
	}
	if !done(one.ID) || column(one.ID) != b.Columns[2].ID || said(one.ID, "Released in acme/shop v1: https://github.com/acme/shop/releases/v1") != 1 {
		t.Fatalf("v1 didn't finish the shipped ticket: %s", column(one.ID))
	}
	if done(two.ID) || done(lost.ID) {
		t.Fatal("a ticket finished with a merged PR still unreleased")
	}
	if p := prs(two.ID); len(p) != 2 || (p[0].ReleasedIn == nil) == (p[1].ReleasedIn == nil) {
		t.Fatalf("released_in: %+v", p)
	}
	// v2 ships #9; already-released PRs aren't compared again
	compares = 0
	if code := release("v2", false); code != 204 || !done(two.ID) || said(two.ID, "Released in acme/shop v2") != 1 {
		t.Fatalf("release v2: %d", code)
	}
	if compares != 3 { // ccc, gone and eee (fff merged with the flag off, so it's never waited on)
		t.Fatalf("compares: %d", compares)
	}
	// a ticket someone reopened after its release isn't touched by a later one
	if err := ts.Move(ctx, "org_pr", one.ID, b.Columns[0].ID, 0); err != nil {
		t.Fatal(err)
	}
	if code := release("v3", false); code != 204 || done(one.ID) {
		t.Fatalf("v3 re-closed a reopened ticket: %d", code)
	}
	// without a Dev complete column, merging finishes as before
	if err := (boards.Store{DB: db}).SetDevComplete(ctx, b.ID, "org_pr", ""); err != nil {
		t.Fatal(err)
	}
	plain, _ := ts.Create(ctx, "org_pr", b.Columns[0].ID, "task", "Plain", "")
	merge("11", "ddd", plain.Key)
	if !done(plain.ID) {
		t.Fatal("merge didn't finish the ticket on a board without Dev complete")
	}
}

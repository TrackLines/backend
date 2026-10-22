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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

// TestPRCommitMentions: a PR also links the tickets its commit messages mention (TL-513), on
// GitHub and Bitbucket; a failed commit fetch still links from the branch, title and description.
func TestPRCommitMentions(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'org_pc'; DELETE FROM projects WHERE owner_clerk_id = 'org_pc';
		DELETE FROM org_integrations WHERE org_id = 'org_pc'`); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/78/access_tokens":
			_, _ = w.Write([]byte(`{"token":"inst"}`))
		case "/repos/acme/web/pulls/5/commits":
			_, _ = w.Write([]byte(`[{"commit":{"message":"CMT-1 schema"}},{"commit":{"message":"CMT-2 api\n\nsee OTH-1 too"}}]`))
		case "/repos/acme/web/pulls/6/commits":
			http.Error(w, "boom", http.StatusBadGateway)
		case "/2.0/repositories/chewed/web/pullrequests/12/commits":
			if r.Header.Get("Authorization") != "Bearer bb-access" {
				http.Error(w, "no", 401)
				return
			}
			_, _ = w.Write([]byte(`{"values":[{"message":"CMT-4 parser"}]}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, 500)
		}
	}))
	defer fake.Close()
	h := System{store: Store{DB: db, Box: box}, admins: organizations.Admins{DB: db}, tickets: tickets.Store{DB: db}, now: time.Now,
		git: Git{GitHubAppID: "1", GitHubAPI: fake.URL, BitbucketAPI: fake.URL + "/2.0",
			GitHubPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}}

	var pid, other string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_pc', 'Web', 'CMT') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_pc', 'Other', 'OTH') RETURNING id`).Scan(&other)
	b, _ := boards.Store{DB: db}.Create(ctx, "org_pc", pid, "web", "")
	ts := tickets.Store{DB: db}
	var tk []string
	for i := range 4 {
		c, err := ts.Create(ctx, "org_pc", b.Columns[0].ID, "task", fmt.Sprintf("t%d", i+1), "")
		if err != nil {
			t.Fatal(err)
		}
		tk = append(tk, c.ID)
	}
	oth, _ := ts.CreateBacklog(ctx, "org_pc", other, "task", "elsewhere", "")
	if err := h.store.connect(ctx, "org_pc", GitHub, "https://api.github.com", "acme", "78", "78", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ConnectAs(ctx, "org_pc", Bitbucket, "https://bitbucket.org", "chewed",
		fmt.Sprintf(`{"access":"bb-access","refresh":"r","expires":%d}`, time.Now().Add(time.Hour).Unix()), "admin"); err != nil {
		t.Fatal(err)
	}
	var bbLink string
	if _, err := db.Exec(ctx, `INSERT INTO project_repos (project_id, org_id, provider, full_name) VALUES ($1, 'org_pc', 'github', 'acme/web')`, pid); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(ctx, `INSERT INTO project_repos (project_id, org_id, provider, full_name) VALUES ($1, 'org_pc', 'bitbucket', 'chewed/web') RETURNING id`, pid).Scan(&bbLink)
	sealedHook, _ := box.SealWithAAD("bb", "bitbucket-hook|"+bbLink)
	if _, err := db.Exec(ctx, `UPDATE project_repos SET webhook_id = '{h}', webhook_secret = $2 WHERE id = $1`, bbLink, sealedHook); err != nil {
		t.Fatal(err)
	}

	sign := func(body, secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	github := func(number int, title string, merged bool) {
		t.Helper()
		body := fmt.Sprintf(`{"action":"x","installation":{"id":78},"repository":{"full_name":"acme/web"},"pull_request":{"number":%d,
			"html_url":"https://github.com/acme/web/pull/%d","title":%q,"body":"","state":"%s","draft":false,"merged":%t,
			"user":{"login":"k"},"head":{"ref":"work"}}}`, number, number, title, map[bool]string{true: "closed", false: "open"}[merged], merged)
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", sign(body, "gh"))
		rec := httptest.NewRecorder()
		h.GitHubWebhook("gh")(rec, req)
		if rec.Code != 204 {
			t.Fatalf("github #%d: %d %s", number, rec.Code, rec.Body)
		}
	}
	linked := func(id string) int {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_prs WHERE ticket_id = $1`, id).Scan(&n)
		return n
	}
	done := func(id string) bool {
		d, _ := ts.Get(ctx, "org_pc", id)
		return d.Done
	}

	// "spikes" names no ticket; its commits name CMT-1 and CMT-2 (and OTH-1, another project's prefix)
	github(5, "spikes", false)
	if linked(tk[0]) != 1 || linked(tk[1]) != 1 || linked(oth.ID) != 0 || linked(tk[2]) != 0 {
		t.Fatalf("commit mentions: %d %d %d %d", linked(tk[0]), linked(tk[1]), linked(oth.ID), linked(tk[2]))
	}
	github(5, "spikes", true)
	if !done(tk[0]) || !done(tk[1]) {
		t.Fatal("merging didn't finish the tickets its commits name")
	}
	// GitHub can't list the commits: the title still links
	github(6, "Fix CMT-3", false)
	if linked(tk[2]) != 1 {
		t.Fatal("a failed commit fetch dropped the title's link")
	}

	// Bitbucket: the commit names CMT-4
	body := `{"repository":{"full_name":"chewed/web"},"pullrequest":{"id":12,"title":"tidy","description":"","state":"OPEN",
		"author":{"display_name":"K"},"source":{"branch":{"name":"tidy"}},"links":{"html":{"href":"https://bitbucket.org/chewed/web/pull-requests/12"}}}}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("X-Event-Key", "pullrequest:created")
	req.Header.Set("X-Hub-Signature", sign(body, "bb"))
	req.SetPathValue("link", bbLink)
	rec := httptest.NewRecorder()
	h.BitbucketWebhook(rec, req)
	if rec.Code != 204 || linked(tk[3]) != 1 {
		t.Fatalf("bitbucket commit mention: %d linked %d", rec.Code, linked(tk[3]))
	}
}

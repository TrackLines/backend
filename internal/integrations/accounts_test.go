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
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

// One Tracklines org, several GitHub accounts (TL-281): each project links repos from whichever GitHub
// org owns them. Connecting another keeps the first; repos come from all; PRs from either link to their
// project's tickets; one can be disconnected (or uninstalled) without the other.
func TestSeveralGitHubAccounts(t *testing.T) {
	ctx := context.Background()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+dbURL[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'org_two'; DELETE FROM org_integrations WHERE org_id = 'org_two';
		DELETE FROM org_admins WHERE org_id = 'org_two'; INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_two', 'admin', 'test')`); err != nil {
		t.Fatal(err)
	}
	var shop, short string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_two', 'Shop', 'SHP') RETURNING id`).Scan(&shop)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_two', 'Short', 'ONE') RETURNING id`).Scan(&short)
	shopTicket, _ := tickets.Store{DB: db}.CreateBacklog(ctx, "org_two", shop, "bug", "Checkout", "")
	shortTicket, _ := tickets.Store{DB: db}.CreateBacklog(ctx, "org_two", short, "bug", "Titles", "")

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	// two GitHub orgs with the app installed: acme (123) and 1tn-pw (456); one person installed both
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/123":
			_, _ = w.Write([]byte(`{"account":{"login":"acme"}}`))
		case r.URL.Path == "/app/installations/456":
			_, _ = w.Write([]byte(`{"account":{"login":"1tn-pw"}}`))
		case r.URL.Path == "/app/installations/123/access_tokens":
			_, _ = w.Write([]byte(`{"token":"ghs_acme"}`))
		case r.URL.Path == "/app/installations/456/access_tokens":
			_, _ = w.Write([]byte(`{"token":"ghs_1tn"}`))
		case r.URL.Path == "/login/oauth/access_token":
			_, _ = w.Write([]byte(`{"access_token":"ghu_owner"}`))
		case r.URL.Path == "/user/installations":
			_, _ = w.Write([]byte(`{"installations":[{"id":123},{"id":456}]}`))
		case r.URL.Path == "/installation/repositories" && r.Header.Get("Authorization") == "Bearer ghs_acme":
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"acme/shop","default_branch":"main"}]}`))
		case r.URL.Path == "/installation/repositories" && r.Header.Get("Authorization") == "Bearer ghs_1tn":
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"1tn-pw/short-service","default_branch":"main"}]}`))
		default:
			http.Error(w, "nope "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer github.Close()
	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	git := Git{GitHubAppID: "4242", GitHubAppSlug: "tracklines", GitHubPrivateKey: pemKey, GitHubAPI: github.URL,
		GitHubClientID: "Iv1.app", GitHubSecret: "app_secret", GitHubWeb: github.URL, now: time.Now}
	h := System{store: Store{DB: db, Box: box}, admins: organizations.Admins{DB: db}, tickets: tickets.Store{DB: db}, git: git, now: time.Now}

	c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "admin"}}
	c.ActiveOrganizationID = "org_two"
	admin := clerk.ContextWithSessionClaims(ctx, c)
	call := func(hf http.HandlerFunc, method, target, body string, path map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(admin)
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	connect := func(installation string) {
		t.Helper()
		var out struct{ URL string }
		_ = json.Unmarshal(call(h.ConnectURL, "GET", "/", "", map[string]string{"provider": "github"}).Body.Bytes(), &out)
		u, _ := url.Parse(out.URL)
		body := `{"state":"` + u.Query().Get("state") + `","installation_id":"` + installation + `","code":"c"}`
		if rec := call(h.Complete, "POST", "/", body, map[string]string{"provider": "github"}); rec.Code != 204 {
			t.Fatalf("connect %s: %d %s", installation, rec.Code, rec.Body)
		}
	}
	accounts := func() []string {
		var out struct{ Accounts []Installation }
		_ = json.Unmarshal(call(h.Connection, "GET", "/", "", map[string]string{"provider": "github"}).Body.Bytes(), &out)
		var names []string
		for _, a := range out.Accounts {
			names = append(names, a.Account)
		}
		return names
	}

	// the second account joins the first, it doesn't replace it; connecting one again changes nothing
	connect("123")
	connect("456")
	connect("123")
	if got := strings.Join(accounts(), ","); got != "1tn-pw,acme" {
		t.Fatalf("accounts: %s", got)
	}
	// repos from both, each linked to its own project
	if rec := call(h.ListRepos, "GET", "/", "", map[string]string{"provider": "github"}); !strings.Contains(rec.Body.String(), "acme/shop") || !strings.Contains(rec.Body.String(), "1tn-pw/short-service") {
		t.Fatalf("repos from both accounts: %s", rec.Body)
	}
	for project, repo := range map[string]string{shop: "acme/shop", short: "1tn-pw/short-service"} {
		if rec := call(h.LinkRepo, "POST", "/", `{"provider":"github","full_name":"`+repo+`"}`, map[string]string{"id": project}); rec.Code != 201 {
			t.Fatalf("link %s: %d %s", repo, rec.Code, rec.Body)
		}
	}
	// a PR from either account links to its project's ticket
	pr := func(installation, repo, key string) int {
		body := `{"action":"opened","installation":{"id":` + installation + `},"repository":{"full_name":"` + repo + `"},
			"pull_request":{"number":1,"html_url":"https://github.com/` + repo + `/pull/1","title":"Fix ` + key + `","body":"","state":"open","draft":false,"merged":false,"user":{"login":"k"},"head":{"ref":"x"}}}`
		mac := hmac.New(sha256.New, []byte("gh"))
		mac.Write([]byte(body))
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()
		h.GitHubWebhook("gh")(rec, req)
		return rec.Code
	}
	linked := func(ticket string) int {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_prs WHERE ticket_id = $1`, ticket).Scan(&n)
		return n
	}
	if pr("123", "acme/shop", "SHP-1") != 204 || pr("456", "1tn-pw/short-service", "ONE-1") != 204 {
		t.Fatal("PR webhooks refused")
	}
	if linked(shopTicket.ID) != 1 || linked(shortTicket.ID) != 1 {
		t.Fatalf("PRs linked: shop %d, short %d", linked(shopTicket.ID), linked(shortTicket.ID))
	}

	// disconnect one account: the other stays, with its repos
	if rec := call(h.Disconnect, "DELETE", "/?installation=123", "", map[string]string{"provider": "github"}); rec.Code != 204 {
		t.Fatalf("disconnect one: %d", rec.Code)
	}
	if got := strings.Join(accounts(), ","); got != "1tn-pw" {
		t.Fatalf("after disconnecting acme: %s", got)
	}
	if rec := call(h.ListRepos, "GET", "/", "", map[string]string{"provider": "github"}); strings.Contains(rec.Body.String(), "acme/shop") || !strings.Contains(rec.Body.String(), "1tn-pw/short-service") {
		t.Fatalf("repos after disconnecting acme: %s", rec.Body)
	}
	if rec := call(h.Disconnect, "DELETE", "/?installation=123", "", map[string]string{"provider": "github"}); rec.Code != 404 {
		t.Fatalf("disconnect a gone account: %d", rec.Code)
	}
	// the app uninstalled from 1tn-pw: that account goes; nothing left
	uninstall := `{"action":"deleted","installation":{"id":456}}`
	mac := hmac.New(sha256.New, []byte("gh"))
	mac.Write([]byte(uninstall))
	req := httptest.NewRequest("POST", "/", strings.NewReader(uninstall))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	h.GitHubWebhook("gh")(httptest.NewRecorder(), req)
	if got := accounts(); len(got) != 0 {
		t.Fatalf("after uninstall: %v", got)
	}
}

package integrations

import (
	"bytes"
	"context"
	"crypto"
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
	"fmt"
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

func TestGitProviders(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id IN ('org_git', 'org_other'); DELETE FROM org_integrations WHERE org_id IN ('org_git', 'org_other');
		DELETE FROM org_admins WHERE org_id IN ('org_git', 'org_other');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_git', 'admin', 'test'), ('org_other', 'admin2', 'test')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_git', 'Shop') RETURNING id`).Scan(&pid)

	// the app's key; the fake GitHub checks app JWTs with the public half
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	validJWT := func(r *http.Request) bool {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			return false
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		return rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) == nil && strings.Contains(string(claims), `"iss":"4242"`)
	}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/123" && validJWT(r):
			_, _ = w.Write([]byte(`{"account":{"login":"acme"}}`))
		case r.URL.Path == "/app/installations/123/access_tokens" && r.Method == "POST" && validJWT(r):
			_, _ = w.Write([]byte(`{"token":"ghs_inst"}`))
		// OAuth during install: the code says who installed it; their installations
		case r.URL.Path == "/login/oauth/access_token" && r.Method == "POST":
			_ = r.ParseForm()
			tokens := map[string]string{"owner_code": "ghu_owner", "stranger_code": "ghu_stranger", "other_owner_code": "ghu_owner2"}
			if t, ok := tokens[r.PostForm.Get("code")]; ok && r.PostForm.Get("client_id") == "Iv1.app" && r.PostForm.Get("client_secret") == "app_secret" {
				_, _ = w.Write([]byte(`{"access_token":"` + t + `"}`))
			} else {
				_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`)) // GitHub answers 200 with an error
			}
		case r.URL.Path == "/user/installations" && r.Header.Get("Authorization") == "Bearer ghu_owner":
			_, _ = w.Write([]byte(`{"installations":[{"id":123},{"id":999}]}`))
		case r.URL.Path == "/user/installations" && r.Header.Get("Authorization") == "Bearer ghu_owner2":
			_, _ = w.Write([]byte(`{"installations":[{"id":123}]}`)) // e.g. a member of the same GitHub org
		case r.URL.Path == "/user/installations" && r.Header.Get("Authorization") == "Bearer ghu_stranger":
			_, _ = w.Write([]byte(`{"installations":[{"id":456}]}`))
		case r.URL.Path == "/installation/repositories" && r.Header.Get("Authorization") == "Bearer ghs_inst":
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"acme/shop","default_branch":"trunk","private":true},{"full_name":"acme/docs","default_branch":"main"}]}`))
		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	defer github.Close()

	// fake Bitbucket: code → tokens (expire in 2h), refresh → new tokens, user, repos, webhooks
	grants := []string{}
	var madeHook map[string]any // the webhook Tracklines asked Bitbucket to create
	deletedHooks := []string{}
	bitbucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		switch {
		case r.URL.Path == "/auth/access_token" && user == "bb_id" && pass == "bb_secret":
			_ = r.ParseForm()
			grants = append(grants, r.Form.Get("grant_type"))
			access := "bb_access"
			if r.Form.Get("grant_type") == "refresh_token" {
				if r.Form.Get("refresh_token") != "bb_refresh" {
					http.Error(w, "bad refresh", 400)
					return
				}
				access = "bb_access2"
			} else if r.Form.Get("code") != "good_code" {
				http.Error(w, "bad code", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "bb_refresh", "expires_in": 7200})
		case r.URL.Path == "/api/user" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer bb_access"):
			_, _ = w.Write([]byte(`{"username":"keloran"}`))
		case r.URL.Path == "/api/repositories/chewed/feed/hooks" && r.Method == "POST" && r.Header.Get("Authorization") == "Bearer bb_access2":
			_ = json.NewDecoder(r.Body).Decode(&madeHook)
			_, _ = w.Write([]byte(`{"uuid":"{hook-1}"}`))
		case strings.HasPrefix(r.URL.Path, "/api/repositories/chewed/feed/hooks/") && r.Method == "DELETE":
			deletedHooks = append(deletedHooks, strings.TrimPrefix(r.URL.Path, "/api/repositories/chewed/feed/hooks/"))
			w.WriteHeader(http.StatusNoContent)
		// TL-286: the person's workspaces, then each workspace's repos (the cross-workspace list is retired)
		case r.URL.Path == "/api/user/workspaces" && r.Header.Get("Authorization") == "Bearer bb_access2":
			_, _ = w.Write([]byte(`{"values":[{"workspace":{"slug":"chewed","name":"Chewed Feed"}}]}`))
		case r.URL.Path == "/api/repositories/chewed" && r.Header.Get("Authorization") == "Bearer bb_access2":
			_, _ = w.Write([]byte(`{"values":[{"full_name":"chewed/feed","is_private":true,"mainbranch":{"name":"master"}}]}`))
		default:
			http.Error(w, "nope "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer bitbucket.Close()

	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	now := time.Now()
	git := Git{GitHubAppID: "4242", GitHubAppSlug: "tracklines", GitHubPrivateKey: pemKey, GitHubAPI: github.URL,
		GitHubClientID: "Iv1.app", GitHubSecret: "app_secret", GitHubWeb: github.URL,
		BitbucketClientID: "bb_id", BitbucketSecret: "bb_secret", BitbucketAuth: bitbucket.URL + "/auth", BitbucketAPI: bitbucket.URL + "/api", PublicAPI: "https://api.example",
		now: func() time.Time { return now }}
	h := System{store: Store{DB: db, Box: box}, admins: organizations.Admins{DB: db}, tickets: tickets.Store{DB: db}, git: git, now: time.Now}

	session := func(org, user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
			c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
			c.ActiveOrganizationID = org
			return c
		}())
	}
	admin := session("org_git", "admin")
	call := func(hf http.HandlerFunc, c context.Context, method, target, body string, path map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(c)
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	stateFrom := func(rec *httptest.ResponseRecorder) string {
		var out struct{ URL string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		u, _ := url.Parse(out.URL)
		return u.Query().Get("state")
	}

	// GitHub: install link → back with the installation id → connected as the account
	rec := call(h.ConnectURL, admin, "GET", "/", "", map[string]string{"provider": "github"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "github.com/apps/tracklines/installations/new") {
		t.Fatalf("github connect url: %d %s", rec.Code, rec.Body)
	}
	state := stateFrom(rec)
	if rec := call(h.ConnectURL, session("org_git", "member"), "GET", "/", "", map[string]string{"provider": "github"}); rec.Code != 403 {
		t.Fatalf("member: %d", rec.Code)
	}
	otherState := stateFrom(call(h.ConnectURL, session("org_other", "admin2"), "GET", "/", "", map[string]string{"provider": "github"}))
	complete := func(c context.Context, provider, body string) int {
		return call(h.Complete, c, "POST", "/", body, map[string]string{"provider": provider}).Code
	}
	if code := complete(admin, "github", `{"state":"`+otherState+`","installation_id":"123","code":"owner_code"}`); code != 400 {
		t.Fatalf("another org's state: %d", code)
	}
	// TL-158: the installation id alone (no proof of who installed it) is refused
	if code := complete(admin, "github", `{"state":"`+state+`","installation_id":"123"}`); code != 400 {
		t.Fatalf("no OAuth code: %d", code)
	}
	if code := complete(admin, "github", `{"state":"`+state+`","installation_id":"123","code":"forged"}`); code != 400 {
		t.Fatalf("bad OAuth code: %d", code)
	}
	// someone who can't access installation 123 can't claim it, even with a valid state
	if code := complete(admin, "github", `{"state":"`+state+`","installation_id":"123","code":"stranger_code"}`); code != 403 {
		t.Fatalf("someone else's installation: %d", code)
	}
	if code := complete(admin, "github", `{"state":"`+state+`","installation_id":"999","code":"owner_code"}`); code != 502 {
		t.Fatalf("not our app's installation: %d", code)
	}
	if code := complete(admin, "github", `{"state":"`+state+`","installation_id":"123","code":"owner_code"}`); code != 204 {
		t.Fatalf("github complete: %d", code)
	}
	// an installation belongs to one org: another org can't connect it too, even with access to it
	if code := complete(session("org_other", "admin2"), "github", `{"state":"`+otherState+`","installation_id":"123","code":"other_owner_code"}`); code != 409 {
		t.Fatalf("installation taken: %d", code)
	}
	if rec := call(h.Connection, admin, "GET", "/", "", map[string]string{"provider": "github"}); !strings.Contains(rec.Body.String(), `"account":"acme"`) {
		t.Fatalf("github connection: %s", rec.Body)
	}
	// repos it can see; link one (visible only, once per org); unlink
	if rec := call(h.ListRepos, admin, "GET", "/", "", map[string]string{"provider": "github"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"full_name":"acme/shop"`) {
		t.Fatalf("github repos: %d %s", rec.Code, rec.Body)
	}
	link := func(body string) int {
		return call(h.LinkRepo, admin, "POST", "/", body, map[string]string{"id": pid}).Code
	}
	if code := link(`{"provider":"github","full_name":"evil/other"}`); code != 400 {
		t.Fatalf("invisible repo: %d", code)
	}
	if code := link(`{"provider":"github","full_name":"ACME/shop"}`); code != 201 {
		t.Fatalf("link: %d", code)
	}
	if code := link(`{"provider":"github","full_name":"acme/shop"}`); code != 409 {
		t.Fatalf("link twice: %d", code)
	}
	if rec := call(h.ProjectRepos, session("org_git", "member"), "GET", "/", "", map[string]string{"id": pid}); !strings.Contains(rec.Body.String(), `"default_branch":"trunk"`) {
		t.Fatalf("project repos: %s", rec.Body)
	}
	if rec := call(h.UnlinkRepo, admin, "DELETE", "/?provider=github&full_name=acme/shop", "", map[string]string{"id": pid}); rec.Code != 204 {
		t.Fatalf("unlink: %d", rec.Code)
	}
	// an expired link is refused
	now = now.Add(20 * time.Minute)
	if code := complete(admin, "github", `{"state":"`+state+`","installation_id":"123","code":"owner_code"}`); code != 400 {
		t.Fatalf("expired state: %d", code)
	}

	// Bitbucket: authorize → code → tokens (sealed) → refreshed when expired
	bbState := stateFrom(call(h.ConnectURL, admin, "GET", "/", "", map[string]string{"provider": "bitbucket"}))
	if code := complete(admin, "bitbucket", `{"state":"`+bbState+`","code":"bad"}`); code != 502 {
		t.Fatalf("bad code: %d", code)
	}
	if code := complete(admin, "bitbucket", `{"state":"`+bbState+`","code":"good_code"}`); code != 204 {
		t.Fatalf("bitbucket complete: %d", code)
	}
	var sealed []byte
	_ = db.QueryRow(ctx, `SELECT secret FROM org_integrations WHERE org_id = 'org_git' AND provider = 'bitbucket'`).Scan(&sealed)
	if bytes.Contains(sealed, []byte("bb_access")) {
		t.Fatal("bitbucket tokens stored in the clear")
	}
	now = now.Add(3 * time.Hour) // past expiry: listing repos refreshes first
	if rec := call(h.ListRepos, admin, "GET", "/", "", map[string]string{"provider": "bitbucket"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"default_branch":"master"`) {
		t.Fatalf("bitbucket repos after refresh: %d %s", rec.Code, rec.Body)
	}
	if strings.Join(grants, ",") != "authorization_code,authorization_code,refresh_token" {
		t.Fatalf("grants: %v", grants)
	}
	// the card says which workspaces the connection reaches (TL-286)
	if rec := call(h.Connection, admin, "GET", "/", "", map[string]string{"provider": "bitbucket"}); !strings.Contains(rec.Body.String(), `"workspaces":[{"slug":"chewed","name":"Chewed Feed"}]`) {
		t.Fatalf("bitbucket workspaces: %s", rec.Body)
	}

	// provider not configured on the server: 503
	off := h
	off.git = Git{}
	if rec := call(off.ConnectURL, admin, "GET", "/", "", map[string]string{"provider": "github"}); rec.Code != 503 {
		t.Fatalf("off: %d", rec.Code)
	}

	// webhooks: signed only; an uninstalled app disconnects the org
	sign := func(body, secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	hook := func(hf http.HandlerFunc, header, event, body, sig string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.Header.Set(header, sig)
		req.Header.Set("X-GitHub-Event", event)
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec.Code
	}
	gh := h.GitHubWebhook("gh_secret")
	ping := `{"zen":"hi"}`
	if code := hook(gh, "X-Hub-Signature-256", "ping", ping, sign(ping, "wrong")); code != 400 {
		t.Fatalf("forged github: %d", code)
	}
	if code := hook(gh, "X-Hub-Signature-256", "ping", ping, sign(ping, "gh_secret")); code != 204 {
		t.Fatalf("github ping: %d", code)
	}
	uninstall := `{"action":"deleted","installation":{"id":123}}`
	if code := hook(gh, "X-Hub-Signature-256", "installation", uninstall, sign(uninstall, "gh_secret")); code != 204 {
		t.Fatalf("uninstall: %d", code)
	}
	if rec := call(h.Connection, admin, "GET", "/", "", map[string]string{"provider": "github"}); !strings.Contains(rec.Body.String(), `"connected":false`) {
		t.Fatalf("still connected after uninstall: %s", rec.Body)
	}
	// TL-165: linking a Bitbucket repo makes its own webhook (our URL naming the link, a fresh secret)
	if rec := call(h.LinkRepo, admin, "POST", "/", `{"provider":"bitbucket","full_name":"chewed/feed"}`, map[string]string{"id": pid}); rec.Code != 201 {
		t.Fatalf("link bitbucket: %d %s", rec.Code, rec.Body)
	}
	var bbLink string
	var stored []byte
	_ = db.QueryRow(ctx, `SELECT id, webhook_secret FROM project_repos WHERE org_id = 'org_git' AND provider = 'bitbucket'`).Scan(&bbLink, &stored)
	secret, _ := madeHook["secret"].(string)
	if madeHook["url"] != "https://api.example/api/webhooks/bitbucket/"+bbLink || len(secret) != 64 || bytes.Contains(stored, []byte(secret)) {
		t.Fatalf("hook: %v (secret stored in the clear: %v)", madeHook, bytes.Contains(stored, []byte(secret)))
	}
	bbHook := func(link, body, sig string) int {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		req.SetPathValue("link", link)
		req.Header.Set("X-Hub-Signature", sig)
		rec := httptest.NewRecorder()
		h.BitbucketWebhook(rec, req)
		return rec.Code
	}
	if code := bbHook(bbLink, ping, sign(ping, secret)); code != 204 {
		t.Fatalf("bitbucket hook: %d", code)
	}
	if code := bbHook(bbLink, ping, sign(ping, "bb_hook")); code != 400 {
		t.Fatalf("someone else's secret: %d", code)
	}
	if code := bbHook("00000000-0000-0000-0000-000000000000", ping, sign(ping, secret)); code != 404 {
		t.Fatalf("unknown link: %d", code)
	}
	// unlinking removes our webhook from the repo; its deliveries then stop being accepted
	if rec := call(h.UnlinkRepo, admin, "DELETE", "/?provider=bitbucket&full_name=chewed/feed", "", map[string]string{"id": pid}); rec.Code != 204 {
		t.Fatalf("unlink bitbucket: %d", rec.Code)
	}
	if len(deletedHooks) != 1 || deletedHooks[0] != "{hook-1}" {
		t.Fatalf("hook not removed: %v", deletedHooks)
	}
	if code := bbHook(bbLink, ping, sign(ping, secret)); code != 404 {
		t.Fatalf("after unlink: %d", code)
	}
	if code := hook(h.GitHubWebhook(""), "X-Hub-Signature-256", "ping", ping, sign(ping, "")); code != 503 {
		t.Fatalf("unconfigured webhook: %d", code)
	}
}

// TL-560: the app key works however it was pasted into the env or 1Password.
func TestPEMText(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	for name, in := range map[string]string{
		"pem":        pemKey,
		"escaped":    strings.ReplaceAll(pemKey, "\n", `\n`),
		"spaces":     strings.ReplaceAll(pemKey, "\n", " "),
		"base64":     base64.StdEncoding.EncodeToString([]byte(pemKey)),
		"whitespace": "\n  " + pemKey + "  \n",
	} {
		g := Git{GitHubAppID: "1", GitHubPrivateKey: pemText(in)}
		if _, err := g.appJWT(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := (Git{GitHubPrivateKey: pemText("not a key")}).appJWT(); err == nil {
		t.Error("a non-key should still fail")
	}
}

// TL-563: every page of an installation's repos, not just the first 100.
func TestGitHubReposPages(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/1/access_tokens" {
			_, _ = w.Write([]byte(`{"token":"ghs_1"}`))
			return
		}
		from, to := 0, 100
		if r.URL.Query().Get("page") == "2" {
			from, to = 100, 150
		}
		var repos []string
		for i := from; i < to; i++ {
			repos = append(repos, fmt.Sprintf(`{"full_name":"acme/r%d","default_branch":"main"}`, i))
		}
		_, _ = fmt.Fprintf(w, `{"total_count":150,"repositories":[%s]}`, strings.Join(repos, ","))
	}))
	defer github.Close()
	repos, err := Git{GitHubAppID: "1", GitHubPrivateKey: pemKey, GitHubAPI: github.URL}.githubRepos(context.Background(), "1")
	if err != nil || len(repos) != 150 || repos[149].FullName != "acme/r149" {
		t.Fatalf("got %d repos, err %v", len(repos), err)
	}
}

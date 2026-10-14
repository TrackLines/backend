package integrations

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	GitHub    = "github"
	Bitbucket = "bitbucket"
)

// Git is the server's GitHub App and Bitbucket OAuth consumer. A provider whose settings are
// missing is off (connecting answers 503). The URLs default to the real services; tests point
// them at fakes.
type Git struct {
	GitHubAppID       string
	GitHubAppSlug     string // github.com/apps/<slug>
	GitHubPrivateKey  string // the app's PEM private key
	GitHubAPI         string // https://api.github.com
	GitHubClientID    string // the App's OAuth client id and secret: the installer authorizes, so we
	GitHubSecret      string // can check the installation is theirs (not just any of our app's)
	GitHubWeb         string // https://github.com (OAuth code exchange)
	BitbucketClientID string
	BitbucketSecret   string
	BitbucketAuth     string // https://bitbucket.org/site/oauth2
	BitbucketAPI      string // https://api.bitbucket.org/2.0
	PublicAPI         string // this API's public address, for webhooks Tracklines creates (https://api.tracklin.es)
	Client            *http.Client
	now               func() time.Time
}

var (
	ErrProviderOff  = errors.New("this provider isn't configured on the server")
	ErrBadState     = errors.New("the connect link has expired or wasn't for this organization; start again")
	ErrProviderCall = errors.New("the provider refused or couldn't be reached")
	ErrNotYours     = errors.New("that GitHub installation isn't one you can access; install the app on your own account or organization")
	ErrTaken        = errors.New("that GitHub installation is already connected to another organization")
)

func (g Git) on(provider string) bool {
	switch provider {
	case GitHub:
		return g.GitHubAppID != "" && g.GitHubAppSlug != "" && g.GitHubPrivateKey != "" && g.GitHubClientID != "" && g.GitHubSecret != ""
	case Bitbucket: // webhooks are made per repo, so it needs our public address too
		return g.BitbucketClientID != "" && g.BitbucketSecret != "" && g.PublicAPI != ""
	}
	return false
}

func (g Git) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return strings.TrimRight(v, "/")
}

func (g Git) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// do sends req and decodes a 2xx JSON answer into out.
func (g Git) do(req *http.Request, out any) error {
	resp, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderCall, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: %s %s → HTTP %d", ErrProviderCall, req.Method, req.URL.Path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// state: what the provider hands back after connecting, sealed so it can't be forged or reused
// for another org, and valid for 15 minutes.
type connectState struct {
	Org      string `json:"o"`
	Provider string `json:"p"`
	Expires  int64  `json:"e"`
}

func (s Store) sealState(org, provider string, now time.Time) (string, error) {
	raw, _ := json.Marshal(connectState{Org: org, Provider: provider, Expires: now.Add(15 * time.Minute).Unix()})
	sealed, err := s.Box.Seal(string(raw))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s Store) openState(state, org, provider string, now time.Time) error {
	sealed, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil {
		return ErrBadState
	}
	raw, err := s.Box.Open(sealed)
	if err != nil {
		return ErrBadState
	}
	var st connectState
	if json.Unmarshal([]byte(raw), &st) != nil || st.Org != org || st.Provider != provider || now.Unix() > st.Expires {
		return ErrBadState
	}
	return nil
}

// GitHub App

// appJWT is the app's own token (RS256, 9 minutes), used to look at installations and mint
// installation tokens.
func (g Git) appJWT() (string, error) {
	block, _ := pem.Decode([]byte(g.GitHubPrivateKey))
	if block == nil {
		return "", errors.New("GITHUB_APP_PRIVATE_KEY isn't a PEM key")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("GITHUB_APP_PRIVATE_KEY must be an RSA key")
		}
		key = rk
	} else {
		return "", errors.New("GITHUB_APP_PRIVATE_KEY can't be parsed")
	}
	now := g.clock()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": g.GitHubAppID})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (g Git) githubRequest(ctx context.Context, method, path, token string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, orDefault(g.GitHubAPI, "https://api.github.com")+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// githubInstallation checks the installation is this app's and returns the account it's on.
func (g Git) githubInstallation(ctx context.Context, id string) (string, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	req, err := g.githubRequest(ctx, http.MethodGet, "/app/installations/"+url.PathEscape(id), jwt, nil)
	if err != nil {
		return "", err
	}
	var inst struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := g.do(req, &inst); err != nil {
		return "", err
	}
	return inst.Account.Login, nil
}

// githubInstalledBy reports whether installation id is one the person who authorized (OAuth code
// from the install redirect) can access: their own, or one in a GitHub org they belong to.
func (g Git) githubInstalledBy(ctx context.Context, code, id string) (bool, error) {
	form := url.Values{"client_id": {g.GitHubClientID}, "client_secret": {g.GitHubSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orDefault(g.GitHubWeb, "https://github.com")+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		Token string `json:"access_token"`
	}
	if err := g.do(req, &tok); err != nil {
		return false, err
	}
	if tok.Token == "" { // GitHub answers 200 with an error body for a bad or used code
		return false, ErrBadState
	}
	// ponytail: up to 10 pages of 100; nobody can access more installations of one app than that
	for page := 1; page <= 10; page++ {
		req, err := g.githubRequest(ctx, http.MethodGet, fmt.Sprintf("/user/installations?per_page=100&page=%d", page), tok.Token, nil)
		if err != nil {
			return false, err
		}
		var out struct {
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		if err := g.do(req, &out); err != nil {
			return false, err
		}
		for _, inst := range out.Installations {
			if strconv.FormatInt(inst.ID, 10) == id {
				return true, nil
			}
		}
		if len(out.Installations) < 100 {
			return false, nil
		}
	}
	return false, nil
}

// githubToken mints an installation token (an hour long).
// ponytail: minted per call; cache it in Valkey until near expiry if PR traffic makes this hot.
func (g Git) githubToken(ctx context.Context, installation string) (string, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	req, err := g.githubRequest(ctx, http.MethodPost, "/app/installations/"+url.PathEscape(installation)+"/access_tokens", jwt, nil)
	if err != nil {
		return "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := g.do(req, &tok); err != nil {
		return "", err
	}
	return tok.Token, nil
}

// Repo is a repository a connection can see.
type Repo struct {
	FullName      string `json:"full_name"` // owner/repo or workspace/repo
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// ponytail: first 100 repos only; page through Link/next headers when an org has more.
func (g Git) githubRepos(ctx context.Context, installation string) ([]Repo, error) {
	token, err := g.githubToken(ctx, installation)
	if err != nil {
		return nil, err
	}
	req, err := g.githubRequest(ctx, http.MethodGet, "/installation/repositories?per_page=100", token, nil)
	if err != nil {
		return nil, err
	}
	var page struct {
		Repositories []struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
			Private       bool   `json:"private"`
		} `json:"repositories"`
	}
	if err := g.do(req, &page); err != nil {
		return nil, err
	}
	out := make([]Repo, 0, len(page.Repositories))
	for _, r := range page.Repositories {
		out = append(out, Repo{FullName: r.FullName, DefaultBranch: r.DefaultBranch, Private: r.Private})
	}
	return out, nil
}

// Bitbucket OAuth

type bitbucketTokens struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expires int64  `json:"expires"`
}

// bitbucketGrant exchanges a code or a refresh token for tokens.
func (g Git) bitbucketGrant(ctx context.Context, form url.Values) (bitbucketTokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orDefault(g.BitbucketAuth, "https://bitbucket.org/site/oauth2")+"/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return bitbucketTokens{}, err
	}
	req.SetBasicAuth(g.BitbucketClientID, g.BitbucketSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := g.do(req, &tok); err != nil {
		return bitbucketTokens{}, err
	}
	return bitbucketTokens{Access: tok.Access, Refresh: tok.Refresh, Expires: g.clock().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()}, nil
}

func (g Git) bitbucketGet(ctx context.Context, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orDefault(g.BitbucketAPI, "https://api.bitbucket.org/2.0")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return g.do(req, out)
}

// bitbucketRepoPath is /repositories/{workspace}/{repo}, each part escaped.
func bitbucketRepoPath(fullName string) string {
	ws, repo, _ := strings.Cut(fullName, "/")
	return "/repositories/" + url.PathEscape(ws) + "/" + url.PathEscape(repo)
}

// bitbucketCreateHook adds a pull-request webhook to a repo, signed with secret; returns its uuid.
func (g Git) bitbucketCreateHook(ctx context.Context, token, fullName, target, secret string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"description": "Tracklines", "url": target, "active": true, "secret": secret,
		"events": []string{"pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orDefault(g.BitbucketAPI, "https://api.bitbucket.org/2.0")+bitbucketRepoPath(fullName)+"/hooks", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		UUID string `json:"uuid"`
	}
	if err := g.do(req, &out); err != nil {
		return "", err
	}
	return out.UUID, nil
}

// bitbucketDeleteHook removes a webhook Tracklines made (unlinking the repo).
func (g Git) bitbucketDeleteHook(ctx context.Context, token, fullName, hook string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, orDefault(g.BitbucketAPI, "https://api.bitbucket.org/2.0")+bitbucketRepoPath(fullName)+"/hooks/"+url.PathEscape(hook), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return g.do(req, nil)
}

// bitbucketAccess returns a working access token for the org, refreshing (and storing) it when
// it's about to expire.
func (h System) bitbucketAccess(ctx context.Context, org string) (string, error) {
	_, secret, err := h.store.credentials(ctx, org, Bitbucket)
	if err != nil {
		return "", err
	}
	var tok bitbucketTokens
	if err := json.Unmarshal([]byte(secret), &tok); err != nil {
		return "", err
	}
	if h.git.clock().Unix() < tok.Expires-60 {
		return tok.Access, nil
	}
	fresh, err := h.git.bitbucketGrant(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}})
	if err != nil {
		return "", err
	}
	if fresh.Refresh == "" {
		fresh.Refresh = tok.Refresh
	}
	c, err := h.store.Get(ctx, org, Bitbucket)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(fresh)
	if err := h.store.ConnectAs(ctx, org, Bitbucket, c.BaseURL, c.Account, string(raw), c.UpdatedBy); err != nil {
		return "", err
	}
	return fresh.Access, nil
}

// ponytail: first 100 repos only; follow "next" when a workspace has more.
func (g Git) bitbucketRepos(ctx context.Context, token string) ([]Repo, error) {
	var page struct {
		Values []struct {
			FullName   string `json:"full_name"`
			IsPrivate  bool   `json:"is_private"`
			MainBranch *struct {
				Name string `json:"name"`
			} `json:"mainbranch"`
		} `json:"values"`
	}
	if err := g.bitbucketGet(ctx, "/repositories?role=member&pagelen=100", token, &page); err != nil {
		return nil, err
	}
	out := make([]Repo, 0, len(page.Values))
	for _, r := range page.Values {
		branch := "main"
		if r.MainBranch != nil && r.MainBranch.Name != "" {
			branch = r.MainBranch.Name
		}
		out = append(out, Repo{FullName: r.FullName, DefaultBranch: branch, Private: r.IsPrivate})
	}
	return out, nil
}

// Repos lists what the org's connection to provider can see.
func (h System) Repos(ctx context.Context, org, provider string) ([]Repo, error) {
	switch provider {
	case GitHub: // every account the org connected (TL-281), each repo once
		list, err := h.store.Installations(ctx, org)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, ErrNotConnected
		}
		var out []Repo
		seen := map[string]bool{}
		for _, i := range list {
			repos, err := h.git.githubRepos(ctx, i.ID)
			if err != nil {
				return nil, err
			}
			for _, r := range repos {
				if k := strings.ToLower(r.FullName); !seen[k] {
					seen[k] = true
					out = append(out, r)
				}
			}
		}
		return out, nil
	case Bitbucket:
		token, err := h.bitbucketAccess(ctx, org)
		if err != nil {
			return nil, err
		}
		return h.git.bitbucketRepos(ctx, token)
	}
	return nil, ErrProviderOff
}

// connectURL is where an admin goes to connect provider for org.
func (h System) connectURL(org, provider string) (string, error) {
	if !h.git.on(provider) {
		return "", ErrProviderOff
	}
	state, err := h.store.sealState(org, provider, h.git.clock())
	if err != nil {
		return "", err
	}
	switch provider {
	case GitHub:
		return "https://github.com/apps/" + url.PathEscape(h.git.GitHubAppSlug) + "/installations/new?state=" + url.QueryEscape(state), nil
	default:
		return orDefault(h.git.BitbucketAuth, "https://bitbucket.org/site/oauth2") + "/authorize?" + url.Values{
			"client_id": {h.git.BitbucketClientID}, "response_type": {"code"}, "state": {state}}.Encode(), nil
	}
}

// complete finishes connecting after the provider sends the admin back: GitHub with the
// installation id, Bitbucket with an OAuth code.
func (h System) complete(ctx context.Context, org, user, provider, state, installationID, code string) error {
	if !h.git.on(provider) {
		return ErrProviderOff
	}
	if err := h.store.openState(state, org, provider, h.git.clock()); err != nil {
		return err
	}
	switch provider {
	case GitHub:
		if _, err := strconv.ParseInt(installationID, 10, 64); err != nil || code == "" {
			return ErrBadState
		}
		// the installation id comes from the browser: it must be one the person installing can access
		// (GitHub's OAuth code from the install redirect proves who they are), not any of our app's
		mine, err := h.git.githubInstalledBy(ctx, code, installationID)
		if err != nil {
			return err
		}
		if !mine {
			return ErrNotYours
		}
		account, err := h.git.githubInstallation(ctx, installationID) // and it must be our app's
		if err != nil {
			return err
		}
		var other string
		if err := h.store.DB.QueryRow(ctx, `SELECT org_id FROM org_integrations WHERE provider = $1 AND external_id = $2 AND org_id <> $3`,
			GitHub, installationID, org).Scan(&other); err == nil {
			return ErrTaken
		}
		return h.store.connect(ctx, org, GitHub, orDefault(h.git.GitHubAPI, "https://api.github.com"), account, installationID, installationID, user)
	case Bitbucket:
		if code == "" {
			return ErrBadState
		}
		tok, err := h.git.bitbucketGrant(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}})
		if err != nil {
			return err
		}
		var me struct {
			Username string `json:"username"`
		}
		if err := h.git.bitbucketGet(ctx, "/user", tok.Access, &me); err != nil {
			return err
		}
		raw, _ := json.Marshal(tok)
		return h.store.ConnectAs(ctx, org, Bitbucket, orDefault(h.git.BitbucketAPI, "https://api.bitbucket.org/2.0"), me.Username, string(raw), user)
	}
	return ErrProviderOff
}

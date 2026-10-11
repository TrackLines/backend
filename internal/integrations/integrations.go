// Package integrations connects an org to other tools. For now Bugfixes: the org stores a Bugfixes
// access token (sealed with the secrets box), and tickets Bugfixes made show the crash report's live
// details. GitHub and Bitbucket will keep their credentials here too.
package integrations

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
)

const Bugfixes = "bugfixes"

// DefaultBugfixesURL is the hosted Bugfixes API.
const DefaultBugfixesURL = "https://api.bugfix.es"

var ErrNotConnected = errors.New("not connected")

type Store struct {
	DB  *pgxpool.Pool
	Box secrets.Box
}

// Connection is what the org has set up for a provider; the secret itself never leaves the server.
type Connection struct {
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	Account   string `json:"account"` // who/where it's connected as (GitHub org, Bitbucket user); "" for Bugfixes
	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

// Connect stores (or replaces) the org's credentials for provider, sealed.
func (s Store) Connect(ctx context.Context, org, provider, baseURL, secret, by string) error {
	return s.ConnectAs(ctx, org, provider, baseURL, "", secret, by)
}

// ConnectAs is Connect that also records the account it's connected as.
func (s Store) ConnectAs(ctx context.Context, org, provider, baseURL, account, secret, by string) error {
	return s.connect(ctx, org, provider, baseURL, account, "", secret, by)
}

// connect stores a connection; externalID is the provider's id for it (GitHub installation id).
func (s Store) connect(ctx context.Context, org, provider, baseURL, account, externalID, secret, by string) error {
	sealed, err := s.Box.Seal(secret)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO org_integrations (org_id, provider, base_url, account, external_id, secret, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (org_id, provider) DO UPDATE SET base_url = $3, account = $4, external_id = $5, secret = $6, updated_by = $7, updated_at = now()`,
		org, provider, baseURL, account, externalID, sealed, by)
	return err
}

func (s Store) Disconnect(ctx context.Context, org, provider string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM org_integrations WHERE org_id = $1 AND provider = $2`, org, provider)
	return err
}

// Get returns the org's connection for provider, or ErrNotConnected.
func (s Store) Get(ctx context.Context, org, provider string) (*Connection, error) {
	c := Connection{Provider: provider}
	err := s.DB.QueryRow(ctx, `SELECT base_url, account, updated_by, to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM org_integrations WHERE org_id = $1 AND provider = $2`, org, provider).Scan(&c.BaseURL, &c.Account, &c.UpdatedBy, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotConnected
	}
	return &c, err
}

// credentials opens the org's secret for provider (server-side use only).
func (s Store) credentials(ctx context.Context, org, provider string) (baseURL, secret string, err error) {
	var sealed []byte
	err = s.DB.QueryRow(ctx, `SELECT base_url, secret FROM org_integrations WHERE org_id = $1 AND provider = $2`, org, provider).Scan(&baseURL, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotConnected
	}
	if err != nil {
		return "", "", err
	}
	secret, err = s.Box.Open(sealed)
	return baseURL, secret, err
}

// HTTP

type System struct {
	store   Store
	admins  organizations.Admins
	reports reportSource
	tickets ticketSource
	git     Git
	now     func() time.Time
}

// Connection: GET /api/integrations/{provider}: whether the org is connected (never the secret).
func (h System) Connection(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	c, err := h.store.Get(r.Context(), org, r.PathValue("provider"))
	if errors.Is(err, ErrNotConnected) {
		httpx.JSON(w, http.StatusOK, map[string]any{"provider": r.PathValue("provider"), "connected": false})
		return
	}
	if err != nil {
		logs.Errorf("integrations: get: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": c.Provider, "connected": true, "base_url": c.BaseURL, "account": c.Account, "updated_by": c.UpdatedBy, "updated_at": c.UpdatedAt})
}

// Connect: PUT /api/integrations/{provider} {"token": "...", "base_url"?}. Org admins only.
func (h System) Connect(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	provider := r.PathValue("provider")
	if provider != Bugfixes {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}
	var in struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Token, in.BaseURL = strings.TrimSpace(in.Token), strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if in.BaseURL == "" {
		in.BaseURL = DefaultBugfixesURL
	}
	if in.Token == "" || len(in.Token) > 500 || !strings.HasPrefix(in.BaseURL, "https://") && !strings.HasPrefix(in.BaseURL, "http://") {
		http.Error(w, "token is required; base_url must be http(s)", http.StatusBadRequest)
		return
	}
	if err := h.store.Connect(r.Context(), org, provider, in.BaseURL, in.Token, user); err != nil {
		if errors.Is(err, secrets.ErrNoKey) {
			http.Error(w, "integrations are not configured on this server", http.StatusServiceUnavailable)
			return
		}
		logs.Errorf("integrations: connect: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Disconnect: DELETE /api/integrations/{provider}. Org admins only.
func (h System) Disconnect(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if err := h.store.Disconnect(r.Context(), org, r.PathValue("provider")); err != nil {
		logs.Errorf("integrations: disconnect: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

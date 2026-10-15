// Package integrations connects an org to other tools. For now Bugfixes: the org stores a Bugfixes
// access token (sealed with the secrets box), and tickets Bugfixes made show the crash report's live
// details. GitHub and Bitbucket will keep their credentials here too.
package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
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
	sealed, err := s.Box.SealWithAAD(secret, secretAAD(org, provider))
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO org_integrations (org_id, provider, base_url, account, external_id, secret, secret_aad_version, updated_by) VALUES ($1, $2, $3, $4, $5, $6, 1, $7)
		ON CONFLICT (org_id, provider, external_id) DO UPDATE SET base_url = $3, account = $4, external_id = $5, secret = $6, secret_aad_version = 1, updated_by = $7, updated_at = now()`,
		org, provider, baseURL, account, externalID, sealed, by)
	return err
}

func secretAAD(org, provider string) string { return org + "|" + provider }

// MigrateSealedSecrets upgrades legacy org_integrations secrets to record-bound encryption.
// The schema migration adds secret_aad_version; rows are marked only after they are re-sealed,
// so this remains safe to retry if startup is interrupted partway through.
func (s Store) MigrateSealedSecrets(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT org_id, provider, external_id, secret FROM org_integrations WHERE secret_aad_version = 0`)
	if err != nil {
		return err
	}
	type legacySecret struct {
		org, provider, externalID string
		sealed                    []byte
	}
	var legacy []legacySecret
	for rows.Next() {
		var row legacySecret
		if err := rows.Scan(&row.org, &row.provider, &row.externalID, &row.sealed); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, row := range legacy {
		plain, err := s.Box.OpenLegacy(row.sealed)
		if err != nil {
			return fmt.Errorf("open legacy integration secret for org %q provider %q: %w", row.org, row.provider, err)
		}
		sealed, err := s.Box.SealWithAAD(plain, secretAAD(row.org, row.provider))
		if err != nil {
			return err
		}
		// one row: an org can have several GitHub installations (TL-281)
		if _, err := s.DB.Exec(ctx, `UPDATE org_integrations SET secret = $4, secret_aad_version = 1 WHERE org_id = $1 AND provider = $2 AND external_id = $3 AND secret_aad_version = 0`,
			row.org, row.provider, row.externalID, sealed); err != nil {
			return err
		}
	}
	return nil
}

// Disconnect removes the org's connection to provider; for GitHub, every account it connected.
func (s Store) Disconnect(ctx context.Context, org, provider string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM org_integrations WHERE org_id = $1 AND provider = $2`, org, provider)
	return err
}

// DisconnectInstallation removes one of the org's GitHub accounts, leaving any others (TL-281).
func (s Store) DisconnectInstallation(ctx context.Context, org, installation string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM org_integrations WHERE org_id = $1 AND provider = 'github' AND external_id = $2`, org, installation)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotConnected
	}
	return err
}

// Installation is one GitHub account (org or user) the org connected, by its App installation.
type Installation struct {
	ID        string `json:"installation_id"`
	Account   string `json:"account"`
	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

// Installations lists the org's GitHub accounts, by name.
func (s Store) Installations(ctx context.Context, org string) ([]Installation, error) {
	rows, err := s.DB.Query(ctx, `SELECT external_id, account, updated_by, to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM org_integrations WHERE org_id = $1 AND provider = 'github' ORDER BY lower(account), external_id`, org)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Installation, error) {
		var i Installation
		return i, r.Scan(&i.ID, &i.Account, &i.UpdatedBy, &i.UpdatedAt)
	})
	if out == nil {
		out = []Installation{}
	}
	return out, err
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
	secret, err = s.Box.OpenWithAAD(sealed, secretAAD(org, provider))
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
	// shortLink finds our short links in PR text (WithShortLinks); nil: only keys and ticket links
	shortLink *regexp.Regexp
}

// Connection: GET /api/integrations/{provider}: whether the org is connected (never the secret).
// Any org member can see it; connecting and disconnecting are for admins.
func (h System) Connection(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if org == "" {
		http.Error(w, "sign in to an organization", http.StatusForbidden)
		return
	}
	if r.PathValue("provider") == GitHub { // several accounts (TL-281)
		list, err := h.store.Installations(r.Context(), org)
		if err != nil {
			logs.Errorf("integrations: get: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out := map[string]any{"provider": GitHub, "connected": len(list) > 0, "accounts": list}
		if len(list) > 0 {
			out["account"] = list[0].Account // kept for older clients
		}
		httpx.JSON(w, http.StatusOK, out)
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
	out := map[string]any{"provider": c.Provider, "connected": true, "base_url": c.BaseURL, "account": c.Account, "updated_by": c.UpdatedBy, "updated_at": c.UpdatedAt}
	if c.Provider == Bitbucket {
		// one person's connection: show which workspaces it reaches (TL-286); left out if Bitbucket can't say
		if token, err := h.bitbucketAccess(r.Context(), org); err == nil {
			if ws, err := h.git.bitbucketWorkspaces(r.Context(), token); err == nil {
				out["workspaces"] = ws
			} else {
				logs.Warnf("integrations: bitbucket workspaces: %v", err)
			}
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Connect: PUT /api/integrations/{provider} {"token": "...", "base_url"?}. Org admins only.
func (h System) Connect(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	org, user, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
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
	if in.Token == "" || len(in.Token) > 500 || !validBaseURL(in.BaseURL) {
		http.Error(w, "token is required; base_url must be an https address like https://bugfix.es", http.StatusBadRequest)
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
	// GitHub ?installation=<id>: just that account, the org's others stay connected (TL-281)
	if id := r.URL.Query().Get("installation"); id != "" && r.PathValue("provider") == GitHub {
		if err := h.store.DisconnectInstallation(r.Context(), org, id); err != nil {
			if errors.Is(err, ErrNotConnected) {
				http.NotFound(w, r)
				return
			}
			logs.Errorf("integrations: disconnect: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.store.Disconnect(r.Context(), org, r.PathValue("provider")); err != nil {
		logs.Errorf("integrations: disconnect: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

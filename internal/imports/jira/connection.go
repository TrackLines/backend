package jira

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/safehttp"
	"github.com/tracklines/backend/internal/secrets"
)

// Lifetime is how long a saved token is kept: 30 days from saving, or from the last extension.
const Lifetime = 30 * 24 * time.Hour

var (
	ErrBadSite      = httpx.Code("jira_site", "enter your Jira Cloud address, like https://your-team.atlassian.net")
	ErrBadLogin     = httpx.Code("jira_login", "enter the email you sign in to Jira with, and an API token")
	ErrNotConnected = httpx.Code("jira_not_connected", "connect Jira first: its token may have expired")
	errNoKey        = httpx.Code("integration_not_configured", "this integration isn't configured on the server")
)

// Connection is an org's Jira site and whose token it imports with, never the token.
type Connection struct {
	Site      string `json:"site"`
	Email     string `json:"email"`
	Account   string `json:"account"` // the Jira user's name
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

var siteName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// SiteURL is a Jira Cloud site as https://<name>.atlassian.net, from that, "<name>.atlassian.net",
// "<name>", or any page on the site; ok false for anything else, so a token only ever goes to
// Atlassian. ponytail: Jira Cloud only (decided 2026-10-20); Server/Data Center would need its own host check.
func SiteURL(raw string) (string, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return "", false
	}
	name := strings.TrimSuffix(u.Hostname(), ".atlassian.net")
	if !siteName.MatchString(name) {
		return "", false
	}
	return "https://" + name + ".atlassian.net", true
}

type Store struct {
	DB  *pgxpool.Pool
	Box secrets.Box
	Now func() time.Time
	// Dial makes the client for a site (tests: a fake Jira, whatever the site)
	Dial func(site, email, token string) Client
}

// NewStore keeps connections in db, tokens sealed with box, talking to Jira over safehttp.
func NewStore(db *pgxpool.Pool, box secrets.Box) Store {
	return Store{DB: db, Box: box, Now: time.Now, Dial: func(site, email, token string) Client {
		return Client{Site: site, Email: email, Token: token, HTTP: safehttp.Client(30 * time.Second)}
	}}
}

func aad(org string) string { return org + "|jira" }

const connCols = `site, email, account, created_by,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

// Get is the org's connection, nil when there's none; an expired one is deleted on the way.
func (s Store) Get(ctx context.Context, org string) (*Connection, error) {
	c, _, err := s.get(ctx, org)
	return c, err
}

func (s Store) get(ctx context.Context, org string) (*Connection, []byte, error) {
	if _, err := s.DB.Exec(ctx, `DELETE FROM jira_connections WHERE org_id = $1 AND expires_at <= $2`, org, s.Now()); err != nil {
		return nil, nil, err
	}
	var c Connection
	var token []byte
	err := s.DB.QueryRow(ctx, `SELECT `+connCols+`, token FROM jira_connections WHERE org_id = $1`, org).
		Scan(&c.Site, &c.Email, &c.Account, &c.CreatedBy, &c.CreatedAt, &c.ExpiresAt, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	return &c, token, err
}

// Connect checks the email and token work on site, then keeps them (replacing any connection) for Lifetime.
func (s Store) Connect(ctx context.Context, org, user, site, email, token string) (*Connection, error) {
	site, ok := SiteURL(site)
	if !ok {
		return nil, ErrBadSite
	}
	email, token = strings.TrimSpace(email), strings.TrimSpace(token)
	if !strings.Contains(email, "@") || token == "" || len(email) > 320 || len(token) > 1024 {
		return nil, ErrBadLogin
	}
	if !s.Box.Ready() {
		return nil, secrets.ErrNoKey
	}
	account, err := s.Dial(site, email, token).Myself(ctx)
	if err != nil {
		return nil, err
	}
	sealed, err := s.Box.SealWithAAD(token, aad(org))
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO jira_connections (org_id, site, email, account, token, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (org_id) DO UPDATE SET site = $2, email = $3, account = $4, token = $5, created_by = $6, created_at = $7, expires_at = $8`,
		org, site, email, account, sealed, user, s.Now(), s.Now().Add(Lifetime)); err != nil {
		return nil, err
	}
	return s.Get(ctx, org)
}

// Extend keeps the token another Lifetime from now.
func (s Store) Extend(ctx context.Context, org string) (*Connection, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE jira_connections SET expires_at = $2 WHERE org_id = $1 AND expires_at > $3`, org, s.Now().Add(Lifetime), s.Now())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotConnected
	}
	return s.Get(ctx, org)
}

// Delete forgets the org's connection and its token.
func (s Store) Delete(ctx context.Context, org string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM jira_connections WHERE org_id = $1`, org)
	return err
}

// Client is the org's Jira, as its connection's person; ErrNotConnected without one.
func (s Store) Client(ctx context.Context, org string) (Client, error) {
	c, sealed, err := s.get(ctx, org)
	if err != nil {
		return Client{}, err
	}
	if c == nil {
		return Client{}, ErrNotConnected
	}
	token, err := s.Box.OpenWithAAD(sealed, aad(org))
	if err != nil {
		return Client{}, err
	}
	return s.Dial(c.Site, c.Email, token), nil
}

// admins is who manages the connection: org admins, signed in (as for imports).
type admins interface {
	RequireAdmin(w http.ResponseWriter, r *http.Request) (org, user string, ok bool)
}

// System is the connection's API: GET, PUT and DELETE /api/imports/jira/connection, and POST …/extend.
type System struct {
	Store  Store
	Admins admins
	On     func() bool // the "imports" flag
}

func (h System) gate(w http.ResponseWriter, r *http.Request) (org, user string, ok bool) {
	if h.On == nil || !h.On() {
		http.NotFound(w, r)
		return "", "", false
	}
	return h.Admins.RequireAdmin(w, r)
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case httpx.Canceled(err):
	case errors.Is(err, secrets.ErrNoKey):
		httpx.Error(w, r, http.StatusServiceUnavailable, errNoKey)
	case errors.Is(err, ErrBadSite), errors.Is(err, ErrBadLogin), errors.Is(err, ErrUnauthorized):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrUnreachable):
		httpx.Error(w, r, http.StatusBadGateway, ErrUnreachable)
	case errors.Is(err, ErrNotConnected):
		httpx.Error(w, r, http.StatusConflict, err)
	default:
		logs.Errorf("jira: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Get: GET /api/imports/jira/connection, the org's connection or null.
func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	c, err := h.Store.Get(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// Connect: PUT /api/imports/jira/connection {"site", "email", "token"}: checked with Jira, then kept for 30 days.
func (h System) Connect(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.gate(w, r)
	if !ok {
		return
	}
	var in struct{ Site, Email, Token string }
	if !httpx.Decode(w, r, &in) {
		return
	}
	c, err := h.Store.Connect(r.Context(), org, user, in.Site, in.Email, in.Token)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// Extend: POST /api/imports/jira/connection/extend, another 30 days from now.
func (h System) Extend(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	c, err := h.Store.Extend(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// Delete: DELETE /api/imports/jira/connection.
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	if err := h.Store.Delete(r.Context(), org); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

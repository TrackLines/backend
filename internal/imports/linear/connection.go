package linear

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
	"github.com/tracklines/backend/internal/safehttp"
	"github.com/tracklines/backend/internal/secrets"
)

// Lifetime is how long a saved key is kept: 30 days from saving or the last extension, as for Jira.
const Lifetime = 30 * 24 * time.Hour

var (
	ErrBadKey       = httpx.Code("linear_key", "enter a Linear personal API key")
	ErrNotConnected = httpx.Code("linear_not_connected", "connect Linear first: its key may have expired")
	errNoKey        = httpx.Code("integration_not_configured", "this integration isn't configured on the server")
)

// Connection is an org's Linear workspace and whose key it imports with, never the key.
type Connection struct {
	Workspace string `json:"workspace"`
	Account   string `json:"account"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

type Store struct {
	DB  *pgxpool.Pool
	Box secrets.Box
	Now func() time.Time
	// Dial makes the client for a key (tests: a fake Linear)
	Dial func(key string) Client
}

// NewStore keeps connections in db, keys sealed with box, talking to Linear over safehttp.
func NewStore(db *pgxpool.Pool, box secrets.Box) Store {
	return Store{DB: db, Box: box, Now: time.Now, Dial: func(key string) Client {
		return Client{Key: key, HTTP: safehttp.Client(60 * time.Second)}
	}}
}

func aad(org string) string { return org + "|linear" }

const connCols = `workspace, account, created_by,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

// Get is the org's connection, nil when there's none; an expired one is deleted on the way.
func (s Store) Get(ctx context.Context, org string) (*Connection, error) {
	c, _, err := s.get(ctx, org)
	return c, err
}

func (s Store) get(ctx context.Context, org string) (*Connection, []byte, error) {
	if _, err := s.DB.Exec(ctx, `DELETE FROM linear_connections WHERE org_id = $1 AND expires_at <= $2`, org, s.Now()); err != nil {
		return nil, nil, err
	}
	var c Connection
	var token []byte
	err := s.DB.QueryRow(ctx, `SELECT `+connCols+`, token FROM linear_connections WHERE org_id = $1`, org).
		Scan(&c.Workspace, &c.Account, &c.CreatedBy, &c.CreatedAt, &c.ExpiresAt, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	return &c, token, err
}

// Connect checks the key works, then keeps it (replacing any connection) for Lifetime.
func (s Store) Connect(ctx context.Context, org, user, key string) (*Connection, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 1024 {
		return nil, ErrBadKey
	}
	if !s.Box.Ready() {
		return nil, secrets.ErrNoKey
	}
	account, workspace, err := s.Dial(key).Viewer(ctx)
	if err != nil {
		return nil, err
	}
	sealed, err := s.Box.SealWithAAD(key, aad(org))
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO linear_connections (org_id, workspace, account, token, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (org_id) DO UPDATE SET workspace = $2, account = $3, token = $4, created_by = $5, created_at = $6, expires_at = $7`,
		org, workspace, account, sealed, user, s.Now(), s.Now().Add(Lifetime)); err != nil {
		return nil, err
	}
	return s.Get(ctx, org)
}

// Extend keeps the key another Lifetime from now.
func (s Store) Extend(ctx context.Context, org string) (*Connection, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE linear_connections SET expires_at = $2 WHERE org_id = $1 AND expires_at > $3`, org, s.Now().Add(Lifetime), s.Now())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotConnected
	}
	return s.Get(ctx, org)
}

// Delete forgets the org's connection and its key.
func (s Store) Delete(ctx context.Context, org string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM linear_connections WHERE org_id = $1`, org)
	return err
}

// Client is the org's Linear, as its connection's person; ErrNotConnected without one.
func (s Store) Client(ctx context.Context, org string) (Client, error) {
	c, sealed, err := s.get(ctx, org)
	if err != nil {
		return Client{}, err
	}
	if c == nil {
		return Client{}, ErrNotConnected
	}
	key, err := s.Box.OpenWithAAD(sealed, aad(org))
	if err != nil {
		return Client{}, err
	}
	return s.Dial(key), nil
}

type admins interface {
	RequireAdmin(w http.ResponseWriter, r *http.Request) (org, user string, ok bool)
}

// System is the connection's API: GET, PUT and DELETE /api/imports/linear/connection, and POST …/extend.
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
	case errors.Is(err, ErrBadKey), errors.Is(err, ErrUnauthorized):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrUnreachable):
		httpx.Error(w, r, http.StatusBadGateway, ErrUnreachable)
	case errors.Is(err, ErrNotConnected):
		httpx.Error(w, r, http.StatusConflict, err)
	default:
		logs.Errorf("linear: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Get: GET /api/imports/linear/connection, the org's connection or null.
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

// Connect: PUT /api/imports/linear/connection {"key"}: checked with Linear, then kept for 30 days.
func (h System) Connect(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.gate(w, r)
	if !ok {
		return
	}
	var in struct{ Key string }
	if !httpx.Decode(w, r, &in) {
		return
	}
	c, err := h.Store.Connect(r.Context(), org, user, in.Key)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// Extend: POST /api/imports/linear/connection/extend, another 30 days from now.
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

// Delete: DELETE /api/imports/linear/connection.
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

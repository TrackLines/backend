// Package embed lets an organisation put its public roadmap and request-form widgets on its own
// sites (TL-494). A widget runs on the customer's page and calls the API from the customer's origin,
// so this package keeps the per-org list of origins that are allowed to do that, and the dynamic CORS
// that reflects Access-Control-Allow-Origin for exactly those origins on the public embed routes.
//
// Everything here is the opt-in extra's backend: the app's own origins still go through the strict,
// app-only CORS unchanged. An org opts in by adding origins; a flags.gg flag gates the feature overall.
package embed

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxOrigins caps a single org's list. ponytail: a sane ceiling, not a product limit; raise it if a
// customer genuinely embeds on more than this many distinct origins.
const MaxOrigins = 50

var (
	ErrInvalid  = errors.New("an origin like https://example.com is required")
	ErrTooMany  = errors.New("too many embed origins for this organization")
	ErrNotFound = errors.New("not found")
)

// Origin is one allowed origin in an org's list.
type Origin struct {
	ID        string `json:"id"`
	Origin    string `json:"origin"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// NormalizeOrigin turns raw into a bare web origin (scheme://host[:port]), lowercased, with no path,
// or ErrInvalid. Only http and https are accepted; this is a trust boundary, so garbage like
// "javascript:…", "*", a bare host or a "null" origin is rejected rather than stored and reflected.
func NormalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", ErrInvalid
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrInvalid
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

type Store struct{ DB *pgxpool.Pool }

const cols = `id::text, origin, created_by, to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Origin, error) {
	var o Origin
	err := row.Scan(&o.ID, &o.Origin, &o.CreatedBy, &o.CreatedAt)
	return o, err
}

// List is org's allowed origins, alphabetically.
func (s Store) List(ctx context.Context, org string) ([]Origin, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM embed_origins WHERE org = $1 ORDER BY origin`, org)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Origin, error) { return scan(row) })
	if out == nil {
		out = []Origin{}
	}
	return out, err
}

// Add normalises raw and adds it to org's list (idempotent: re-adding an existing origin is fine and
// returns it). ErrTooMany once the list is full, unless the origin is already there.
func (s Store) Add(ctx context.Context, org, by, raw string) (Origin, error) {
	origin, err := NormalizeOrigin(raw)
	if err != nil {
		return Origin{}, err
	}
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM embed_origins WHERE org = $1`, org).Scan(&n); err != nil {
		return Origin{}, err
	}
	if n >= MaxOrigins {
		here, err := s.Allowed(ctx, org, origin)
		if err != nil {
			return Origin{}, err
		}
		if !here {
			return Origin{}, ErrTooMany
		}
	}
	return scan(s.DB.QueryRow(ctx, `INSERT INTO embed_origins (org, origin, created_by) VALUES ($1, $2, $3)
		ON CONFLICT (org, origin) DO UPDATE SET origin = EXCLUDED.origin
		RETURNING `+cols, org, origin, by))
}

// Remove drops origin id from org's list. ErrNotFound if it isn't the org's.
func (s Store) Remove(ctx context.Context, org, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM embed_origins WHERE id::text = $1 AND org = $2`, id, org)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Allowed reports whether origin is in org's list (exact match on the normalised form).
func (s Store) Allowed(ctx context.Context, org, origin string) (bool, error) {
	var ok bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM embed_origins WHERE org = $1 AND origin = $2)`, org, origin).Scan(&ok)
	return ok, err
}

// Resolver maps a public embed request's path to the organisation that owns the resource, so its
// allow-list can be checked without a session.
type Resolver struct{ DB *pgxpool.Pool }

// match picks the embeddable resource a path addresses, mirroring the routes in service.go:
// GET /api/roadmaps/{id}(/updates) and GET|POST /api/intake/{token}. "" means not an embed route.
func match(path string) (kind, key string) {
	switch {
	case strings.HasPrefix(path, "/api/roadmaps/"):
		rest := strings.TrimSuffix(strings.TrimPrefix(path, "/api/roadmaps/"), "/updates")
		if rest == "" || strings.Contains(rest, "/") {
			return "", ""
		}
		return "roadmap", rest
	case strings.HasPrefix(path, "/api/intake/"):
		rest := strings.TrimPrefix(path, "/api/intake/")
		if rest == "" || strings.Contains(rest, "/") {
			return "", ""
		}
		return "intake", rest
	}
	return "", ""
}

// OrgFor returns the org that owns the resource addressed by path, "" if path isn't an embed route or
// the resource doesn't exist. A missing resource is not an error: the caller just falls back to strict CORS.
func (r Resolver) OrgFor(ctx context.Context, path string) (string, error) {
	kind, key := match(path)
	var q, org string
	switch kind {
	case "roadmap":
		q = `SELECT p.owner_clerk_id FROM roadmaps rm JOIN projects p ON p.id = rm.project_id WHERE rm.id::text = $1`
	case "intake":
		q = `SELECT owner_clerk_id FROM projects WHERE intake_token = $1`
	default:
		return "", nil
	}
	err := r.DB.QueryRow(ctx, q, key).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return org, err
}

// CORS wraps the strict (app-only) CORS so that a request from a third-party origin that is in the
// owning org's allow-list gets Access-Control-Allow-Origin reflected and its preflight answered; an
// allowed non-preflight request is served through `pass` (the mux directly) so the strict CORS, which
// 403s unknown origins, never sees it. Everything else — no Origin, an app origin, the feature off, an
// unknown origin, a non-embed path — goes to `strict` exactly as before.
// record, when set, is called for an allowed, non-preflight embed request that carries a data-key
// (the `embed` query param), to attribute the hit for usage stats (TL-497). It must not block.
func CORS(appOrigins []string, enabled func() bool,
	orgFor func(context.Context, string) (string, error),
	allowed func(context.Context, string, string) (bool, error),
	record func(org, origin, path, key string),
	methods, headers string) func(strict, pass http.Handler) http.Handler {
	app := make(map[string]bool, len(appOrigins))
	for _, o := range appOrigins {
		app[o] = true
	}
	return func(strict, pass http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" || app[origin] || enabled == nil || !enabled() {
				strict.ServeHTTP(w, r)
				return
			}
			org, err := orgFor(r.Context(), r.URL.Path)
			if err != nil || org == "" {
				strict.ServeHTTP(w, r)
				return
			}
			ok, err := allowed(r.Context(), org, origin)
			if err != nil || !ok {
				strict.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", methods)
			h.Set("Access-Control-Allow-Headers", headers)
			h.Set("Access-Control-Max-Age", "86400")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if key := r.URL.Query().Get("embed"); key != "" && record != nil {
				record(org, origin, r.URL.Path, key)
			}
			pass.ServeHTTP(w, r)
		})
	}
}

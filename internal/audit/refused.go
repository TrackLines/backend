package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
)

// Routes is the router, for the pattern a request matched (mux.Handler).
type Routes interface {
	Handler(r *http.Request) (http.Handler, string)
}

// Refused records a change someone wasn't allowed to make (TL-700, owner 27 Oct): a signed-in caller in an
// org gets 403 on a POST, PUT, PATCH or DELETE. A change that goes through is already in the log (its
// triggers, in its own transaction), so only refusals are recorded here; reads, bad input (400) and
// failures (500, which Bugfixes hears about) aren't. Paid plans only, like the rest of the org log.
func Refused(db *pgxpool.Pool, routes Routes) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				next.ServeHTTP(w, r)
				return
			}
			user, signedIn := auth.UserID(r.Context())
			org := auth.OrgID(r.Context())
			if !signedIn || org == "" {
				next.ServeHTTP(w, r)
				return
			}
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status != http.StatusForbidden {
				return
			}
			_, pattern := routes.Handler(r)
			var coded struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.body.Bytes(), &coded)
			data, _ := json.Marshal(map[string]string{"method": r.Method, "route": pattern, "path": r.URL.Path, "code": coded.Code})
			// the answer has gone: record it without holding up the caller's next request, nor failing it
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			if _, err := db.Exec(ctx, `SELECT audit_org($1, 'access.refused', NULL, NULL, NULL, $2::jsonb, $3)`, org, string(data), auth.ActorID(r.Context())); err != nil {
				logs.Errorf("audit: refused %s %s by %s: %v", r.Method, pattern, user, err)
			}
		})
	}
}

// statusRecorder notes the status and keeps the start of a 403's body, for its error code.
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.status == http.StatusForbidden && s.body.Len() < 512 {
		s.body.Write(b[:min(len(b), 512-s.body.Len())])
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ExpireKeys records each API key that's passed its expiry since the last sweep (TL-700), once: the key's
// row is marked as it's recorded. A revoked key isn't recorded again (its revoking was). Paid plans only.
func ExpireKeys(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	tag, err := db.Exec(ctx, `WITH expired AS (
			UPDATE api_keys SET expiry_recorded = true WHERE expires_at <= now() AND NOT expiry_recorded
			RETURNING id, org_id, kind, name, owner_clerk_id, read_only, expires_at, revoked_at)
		SELECT audit_org(org_id, 'api_key.expired', 'api_key', id::text, NULL, jsonb_build_object('kind', kind, 'name', name,
			'owner', owner_clerk_id, 'read_only', read_only, 'expires_at', expires_at))
		FROM expired WHERE revoked_at IS NULL`)
	return tag.RowsAffected(), err
}

// RunExpireKeys sweeps every interval until ctx ends.
func RunExpireKeys(ctx context.Context, db *pgxpool.Pool, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := ExpireKeys(ctx, db); err != nil && ctx.Err() == nil {
			logs.Errorf("audit: expired keys: %v", err)
		} else if n > 0 {
			logs.Logf("audit: recorded %d expired API key(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

package embed

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MaxLabel caps a key's label.
const MaxLabel = 100

// Key is a public embed identifier with the usage recorded against it. The token is not a secret: it
// identifies an embed/placement for stats, it does not grant access (that's the origin allow-list).
type Key struct {
	ID        string  `json:"id"`
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	CreatedBy string  `json:"created_by"`
	CreatedAt string  `json:"created_at"`
	Hits      int     `json:"hits"`
	Origins   int     `json:"origins"`
	LastUsed  *string `json:"last_used"` // YYYY-MM-DD, null if never used
}

// Usage is one key's hits for a single site and resource.
type Usage struct {
	Origin   string `json:"origin"`
	Resource string `json:"resource"`
	Hits     int    `json:"hits"`
	LastUsed string `json:"last_used"`
}

func newKey() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "tle_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateKey mints a new public embed key for project with the given label.
func (s Store) CreateKey(ctx context.Context, org, project, by, label string) (Key, error) {
	label = strings.TrimSpace(label)
	if len([]rune(label)) > MaxLabel {
		return Key{}, ErrInvalid
	}
	tok, err := newKey()
	if err != nil {
		return Key{}, err
	}
	var k Key
	err = s.DB.QueryRow(ctx, `INSERT INTO embed_keys (org, project_id, key, label, created_by) VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, key, label, created_by, to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		org, project, tok, label, by).Scan(&k.ID, &k.Key, &k.Label, &k.CreatedBy, &k.CreatedAt)
	return k, err
}

// ListKeys is project's keys, newest first, each with its usage totals so far.
func (s Store) ListKeys(ctx context.Context, project string) ([]Key, error) {
	rows, err := s.DB.Query(ctx, `SELECT k.id::text, k.key, k.label, k.created_by,
			to_char(k.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(SUM(u.hits), 0)::bigint, COUNT(DISTINCT u.origin)::bigint,
			to_char(MAX(u.day), 'YYYY-MM-DD')
		FROM embed_keys k LEFT JOIN embed_usage u ON u.embed_key_id = k.id
		WHERE k.project_id = $1 GROUP BY k.id ORDER BY k.created_at DESC`, project)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) {
		var k Key
		var hits, origins int64
		err := row.Scan(&k.ID, &k.Key, &k.Label, &k.CreatedBy, &k.CreatedAt, &hits, &origins, &k.LastUsed)
		k.Hits, k.Origins = int(hits), int(origins)
		return k, err
	})
	if out == nil {
		out = []Key{}
	}
	return out, err
}

// DeleteKey removes key id from project, and its usage by cascade. ErrNotFound if it isn't the project's.
func (s Store) DeleteKey(ctx context.Context, project, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM embed_keys WHERE id::text = $1 AND project_id = $2`, id, project)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// KeyUsage is key id's hits grouped by site and resource, busiest first. ErrNotFound if not project's.
func (s Store) KeyUsage(ctx context.Context, project, id string) ([]Usage, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM embed_keys WHERE id::text = $1 AND project_id = $2)`, id, project).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT u.origin, u.resource, SUM(u.hits)::bigint, to_char(MAX(u.day), 'YYYY-MM-DD')
		FROM embed_usage u WHERE u.embed_key_id::text = $1
		GROUP BY u.origin, u.resource ORDER BY SUM(u.hits) DESC, u.origin`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Usage, error) {
		var u Usage
		var hits int64
		err := row.Scan(&u.Origin, &u.Resource, &hits, &u.LastUsed)
		u.Hits = int(hits)
		return u, err
	})
	if out == nil {
		out = []Usage{}
	}
	return out, err
}

// RecordUsage bumps the counter for a widget request from origin to path carrying key — but only when
// key is a live key of the project the resource belongs to, and path is an embed resource. Best-effort:
// it's stats, so errors are dropped.
func (s Store) RecordUsage(ctx context.Context, project, origin, path, key string) {
	kind, _ := match(path)
	if kind == "" || key == "" {
		return
	}
	_, _ = s.DB.Exec(ctx, `INSERT INTO embed_usage (embed_key_id, origin, resource, day, hits)
		SELECT k.id, $2, $3, current_date, 1 FROM embed_keys k WHERE k.project_id::text = $1 AND k.key = $4
		ON CONFLICT (embed_key_id, origin, resource, day) DO UPDATE SET hits = embed_usage.hits + 1`,
		project, origin, kind, key)
}

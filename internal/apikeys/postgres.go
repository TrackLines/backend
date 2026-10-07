package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Prefix marks tracklines API keys, so the auth middleware can tell them from Clerk JWTs.
const Prefix = "tl_"

const (
	KindAI      = "ai"
	KindService = "service"
)

var ErrNotFound = errors.New("api key not found")

type Key struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

type Store struct{ DB *pgxpool.Pool }

func hash(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

const cols = `id, name, kind, prefix,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(last_used_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.Name, &k.Kind, &k.Prefix, &k.CreatedAt, &k.LastUsedAt)
	return k, err
}

// Create mints a key for owner. The plaintext is returned once and never stored.
func (s Store) Create(ctx context.Context, owner, name, kind string) (string, *Key, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	plain := Prefix + base64.RawURLEncoding.EncodeToString(b)
	k, err := scan(s.DB.QueryRow(ctx, `INSERT INTO api_keys (owner_clerk_id, name, kind, prefix, hash)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+cols, owner, name, kind, plain[:10], hash(plain)))
	if err != nil {
		return "", nil, err
	}
	return plain, &k, nil
}

// List returns the owner's active keys.
func (s Store) List(ctx context.Context, owner string) ([]Key, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM api_keys
		WHERE owner_clerk_id = $1 AND revoked_at IS NULL ORDER BY created_at`, owner)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) { return scan(row) })
	if out == nil {
		out = []Key{}
	}
	return out, err
}

func (s Store) Revoke(ctx context.Context, id, owner string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE api_keys SET revoked_at = now()
		WHERE id = $1 AND owner_clerk_id = $2 AND revoked_at IS NULL`, id, owner)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "22P02" { // malformed uuid
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Owner resolves an active key to its owner and stamps last_used_at.
// ponytail: one UPDATE per API request; batch the stamp if key traffic ever gets heavy.
func (s Store) Owner(ctx context.Context, plain string) (string, string, string, error) {
	var owner, name, kind string
	err := s.DB.QueryRow(ctx, `UPDATE api_keys SET last_used_at = now()
		WHERE hash = $1 AND revoked_at IS NULL RETURNING owner_clerk_id, name, kind`, hash(plain)).Scan(&owner, &name, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	return owner, name, kind, err
}

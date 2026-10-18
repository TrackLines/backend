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

// Prefix marks tracklines API keys, so the auth middleware can tell them from Clerk JWTs. Keys say
// their kind too, so people can tell them apart (TL-271): tl_ai_… for agents, tl_s_… for services;
// keys made before that are plain tl_… and still work. The kind that counts is the stored one: the
// whole key, prefix included, is what's hashed, so an edited prefix is just a wrong key.
const (
	Prefix        = "tl_"
	PrefixAI      = "tl_ai_"
	PrefixService = "tl_s_"
)

const (
	KindAI      = "ai"
	KindService = "service"
)

var (
	ErrNotFound = errors.New("api key not found")
	ErrExpired  = errors.New("api key expired")
)

// Key lifetimes (TL-433): 90 days unless the creator picks otherwise, at most a year and a day.
const (
	DefaultDays = 90
	MaxDays     = 366
)

type Key struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	Prefix     string  `json:"prefix"`
	Client     *string `json:"client"`  // agent keys: the client the agent runs in (TL-269)
	Label      *string `json:"label"`   // "other" agents: what they're called
	Display    string  `json:"display"` // what it's called: "Claude", or the service key's name
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	ExpiresAt  string  `json:"expires_at"`
}

type Store struct{ DB *pgxpool.Pool }

func hash(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

const cols = `id, name, kind, prefix, client, label,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(last_used_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.Name, &k.Kind, &k.Prefix, &k.Client, &k.Label, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt)
	k.Display = k.Name
	if k.Client != nil {
		k.Display = DisplayName(*k.Client, value(k.Label))
	}
	return k, err
}

// Create mints a key for owner that works for days days. The plaintext is returned once and never
// stored. client and label are for agent keys (nil for service keys).
func (s Store) Create(ctx context.Context, owner, org, name, kind string, client, label *string, days int) (string, *Key, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	start := PrefixService
	if kind == KindAI {
		start = PrefixAI
	}
	plain := start + base64.RawURLEncoding.EncodeToString(b)
	k, err := scan(s.DB.QueryRow(ctx, `INSERT INTO api_keys (owner_clerk_id, org_id, name, kind, prefix, hash, client, label, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + make_interval(days => $9)) RETURNING `+cols, owner, org, name, kind, plain[:12], hash(plain), client, label, days))
	if err != nil {
		return "", nil, err
	}
	return plain, &k, nil
}

// List returns the owner's active keys.
// List is owner's live keys in org (the org they act in).
func (s Store) List(ctx context.Context, owner, org string) ([]Key, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM api_keys
		WHERE owner_clerk_id = $1 AND org_id = $2 AND revoked_at IS NULL ORDER BY created_at`, owner, org)
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
// Owner looks up a live key: its owner, the org it acts in ("" for pre-org keys), name and kind.
// RevokeKey revokes the key with this plaintext (its owner left the org).
func (s Store) RevokeKey(ctx context.Context, plain string) error {
	_, err := s.DB.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE hash = $1 AND revoked_at IS NULL`, hash(plain))
	return err
}

// An expired key is ErrExpired, and its last use isn't stamped.
func (s Store) Owner(ctx context.Context, plain string) (owner, org, name, kind string, err error) {
	var expired bool
	err = s.DB.QueryRow(ctx, `UPDATE api_keys SET last_used_at = CASE WHEN expires_at > now() THEN now() ELSE last_used_at END
		WHERE hash = $1 AND revoked_at IS NULL RETURNING owner_clerk_id, COALESCE(org_id, ''), name, kind, expires_at <= now()`,
		hash(plain)).Scan(&owner, &org, &name, &kind, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", "", ErrNotFound
	}
	if err == nil && expired {
		return "", "", "", "", ErrExpired
	}
	return owner, org, name, kind, err
}

// NameTakenByOther: another member has a live key in org with this name (any case). One person may
// reuse a name across their own keys; two people sharing one would act as each other.
// ponytail: checked before insert, not a unique index; a same-instant race by two members is the gap.
func (s Store) NameTakenByOther(ctx context.Context, org, owner, name string) (bool, error) {
	var taken bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys
		WHERE org_id = $1 AND owner_clerk_id <> $2 AND lower(name) = lower($3) AND revoked_at IS NULL)`, org, owner, name).Scan(&taken)
	return taken, err
}

// NameInUse: any key in org, live or revoked, already has this name (any case). Generated agent
// names are never reused, so old work can't be mistaken for a new key's.
func (s Store) NameInUse(ctx context.Context, org, name string) (bool, error) {
	var used bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE org_id = $1 AND lower(name) = lower($2))`, org, name).Scan(&used)
	return used, err
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

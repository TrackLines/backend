package bugfixesTickets

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BUGFIXES_KEY_PREFIX is the prefix Bugfixes API keys use so the auth middleware
// can tell them from Clerk JWTs (tl_) and from other agent keys.
const BUGFIXES_KEY_PREFIX = "bf_"

var (
	ErrNotFound = errors.New("not found")
)

type Ticket struct {
	ID        string   `json:"id"`
	BoardID   string   `json:"board_id"`
	ColumnID  string   `json:"column_id"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Priority  string   `json:"priority"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
	Labels    []string `json:"labels"` // e.g. "agent:checkout-api"
}

type Store struct{ DB *pgxpool.Pool }

// Owner resolves a bugfixes key to its owner + name, stamping last_used_at.
// Lookup is by sha256 hash (same pattern as apikeys.Owner).
func (s Store) Owner(ctx context.Context, plain string) (string, string, error) {
	h := sha256.Sum256([]byte(plain))
	var owner, name string
	err := s.DB.QueryRow(ctx, `
		UPDATE api_keys SET last_used_at = now()
		WHERE hash = $1 AND revoked_at IS NULL
		RETURNING owner_clerk_id, name
	`, h[:]).Scan(&owner, &name)
	if err != nil {
		return "", "", ErrNotFound
	}
	return owner, name, nil
}

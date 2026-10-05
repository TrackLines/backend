package users

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ DB *pgxpool.Pool }

// Upsert creates the user row or refreshes its email.
func (s Store) Upsert(ctx context.Context, clerkID, email string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ($1, $2)
		ON CONFLICT (clerk_id) DO UPDATE SET email = EXCLUDED.email, updated_at = now()`, clerkID, email)
	return err
}

func (s Store) Exists(ctx context.Context, clerkID string) (bool, error) {
	var ok bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE clerk_id = $1)`, clerkID).Scan(&ok)
	return ok, err
}

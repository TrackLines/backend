package billing

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Customer returns the user's Stripe customer id, creating and storing one first.
// If two requests race, the first stored id wins (Stripe idempotency makes them the same anyway).
func Customer(ctx context.Context, db *pgxpool.Pool, s Stripe, clerkID string) (string, error) {
	var id *string
	var email string
	if err := db.QueryRow(ctx, `SELECT stripe_customer_id, email FROM users WHERE clerk_id = $1`, clerkID).Scan(&id, &email); err != nil {
		return "", err
	}
	if id != nil {
		return *id, nil
	}
	created, err := s.CreateCustomer(ctx, clerkID, email)
	if err != nil {
		return "", err
	}
	var stored string
	err = db.QueryRow(ctx, `UPDATE users SET stripe_customer_id = COALESCE(stripe_customer_id, $2), updated_at = now()
		WHERE clerk_id = $1 RETURNING stripe_customer_id`, clerkID, created).Scan(&stored)
	return stored, err
}

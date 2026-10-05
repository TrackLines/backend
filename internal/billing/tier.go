package billing

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FreeBoardLimit is how many boards a free-tier user may own.
const FreeBoardLimit = 1

// IsPaid reports whether the user's Stripe subscription is live.
func IsPaid(ctx context.Context, db *pgxpool.Pool, clerkID string) (bool, error) {
	var paid bool
	err := db.QueryRow(ctx, `SELECT COALESCE(bool_or(stripe_status IN ('active', 'trialing')), false)
		FROM users WHERE clerk_id = $1`, clerkID).Scan(&paid)
	return paid, err
}

// BoardLimit returns the board cap for the user: -1 (unlimited) when paid.
func BoardLimit(ctx context.Context, db *pgxpool.Pool, clerkID string) (int, error) {
	paid, err := IsPaid(ctx, db, clerkID)
	if err != nil || paid {
		return -1, err
	}
	return FreeBoardLimit, nil
}

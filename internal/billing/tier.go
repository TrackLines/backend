package billing

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FreeProjectLimit is how many projects a free-plan organization may have (boards inside are unlimited).
const FreeProjectLimit = 1

// IsPaid reports whether the organization's plan is live (TL-243: the plan belongs to the org).
func IsPaid(ctx context.Context, db *pgxpool.Pool, org string) (bool, error) {
	var paid bool
	err := db.QueryRow(ctx, `SELECT COALESCE(bool_or(stripe_status IN ('active', 'trialing')), false)
		FROM org_billing WHERE org_id = $1`, org).Scan(&paid)
	return paid, err
}

// ProjectLimit returns the organization's project cap: -1 (unlimited) when paid.
func ProjectLimit(ctx context.Context, db *pgxpool.Pool, org string) (int, error) {
	paid, err := IsPaid(ctx, db, org)
	if err != nil || paid {
		return -1, err
	}
	return FreeProjectLimit, nil
}

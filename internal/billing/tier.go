package billing

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Plans (TL-290): Free 1 project, Pro 5, Enterprise unlimited. Boards inside a project are unlimited.
const (
	Free       = "free"
	Pro        = "pro"
	Enterprise = "enterprise"

	FreeProjectLimit = 1
	ProProjectLimit  = 5
)

// limits is each plan's project cap; -1 is unlimited.
var limits = map[string]int{Free: FreeProjectLimit, Pro: ProProjectLimit, Enterprise: -1}

// IsPaid reports whether the organization's plan is live (TL-243: the plan belongs to the org).
func IsPaid(ctx context.Context, db *pgxpool.Pool, org string) (bool, error) {
	plan, err := PlanOf(ctx, db, org)
	return plan != Free, err
}

// PlanOf is the organization's plan: its paid tier while the subscription (or a hand grant) is live,
// otherwise Free. A live plan with no tier recorded predates tiers and was unlimited: Enterprise.
func PlanOf(ctx context.Context, db *pgxpool.Pool, org string) (string, error) {
	var plan *string
	var live bool
	err := db.QueryRow(ctx, `SELECT plan, stripe_status IN ('active', 'trialing') FROM org_billing WHERE org_id = $1`, org).Scan(&plan, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return Free, nil
	}
	if err != nil {
		return Free, err
	}
	if !live {
		return Free, nil
	}
	if plan == nil {
		return Enterprise, nil
	}
	return *plan, nil
}

// ProjectLimit returns the organization's project cap: -1 is unlimited.
func ProjectLimit(ctx context.Context, db *pgxpool.Pool, org string) (int, error) {
	plan, err := PlanOf(ctx, db, org)
	if err != nil {
		return FreeProjectLimit, err
	}
	return limits[plan], nil
}

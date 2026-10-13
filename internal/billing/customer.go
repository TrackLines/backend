package billing

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Customer returns the organization's Stripe customer id, creating and storing one first, billed to
// email (the admin paying). If two requests race, the first stored id wins.
func Customer(ctx context.Context, db *pgxpool.Pool, s Stripe, org, email string) (string, error) {
	var id *string
	if err := db.QueryRow(ctx, `INSERT INTO org_billing (org_id, billing_email) VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET billing_email = CASE WHEN org_billing.billing_email = '' THEN EXCLUDED.billing_email ELSE org_billing.billing_email END
		RETURNING stripe_customer_id`, org, email).Scan(&id); err != nil {
		return "", err
	}
	if id != nil {
		return *id, nil
	}
	created, err := s.CreateCustomer(ctx, org, email)
	if err != nil {
		return "", err
	}
	var stored string
	err = db.QueryRow(ctx, `UPDATE org_billing SET stripe_customer_id = COALESCE(stripe_customer_id, $2), updated_at = now()
		WHERE org_id = $1 RETURNING stripe_customer_id`, org, created).Scan(&stored)
	return stored, err
}

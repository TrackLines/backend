package integrations

import (
	"context"
	"crypto/sha256"
)

// firstDelivery claims a signed webhook body: true the first time, false for a replay. Call
// forgetDelivery if handling it then fails, so the provider's retry is processed (TL-170).
func (h System) firstDelivery(ctx context.Context, provider string, body []byte) (bool, error) {
	sum := sha256.Sum256(body)
	tag, err := h.store.DB.Exec(ctx, `INSERT INTO webhook_deliveries (provider, body_hash) VALUES ($1, $2) ON CONFLICT DO NOTHING`, provider, sum[:])
	if err != nil {
		return false, err
	}
	_, _ = h.store.DB.Exec(ctx, `DELETE FROM webhook_deliveries WHERE received_at < now() - interval '180 days'`)
	return tag.RowsAffected() == 1, nil
}

func (h System) forgetDelivery(ctx context.Context, provider string, body []byte) {
	sum := sha256.Sum256(body)
	_, _ = h.store.DB.Exec(ctx, `DELETE FROM webhook_deliveries WHERE provider = $1 AND body_hash = $2`, provider, sum[:])
}

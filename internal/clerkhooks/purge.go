package clerkhooks

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/retention"
)

// Retention is how long a deleted organization's data is kept before it's purged.
const Retention = retention.Retention

// orgData deletes everything an organization owns (retention.OrgData).
var orgData = retention.OrgData

// Purge permanently deletes the data of organizations deleted more than Retention before now
// (retention.PurgeOrgs: the automation service runs it nightly, TL-431).
func Purge(ctx context.Context, db *pgxpool.Pool, now time.Time) (int, error) {
	return retention.PurgeOrgs(ctx, db, now)
}

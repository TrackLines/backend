package billing

import (
	"context"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OrgUpdater changes a Clerk organization (organization.Client in production).
type OrgUpdater interface {
	Update(ctx context.Context, id string, params *organization.UpdateParams) (*clerk.Organization, error)
}

// SyncMemberCap sets Clerk's own cap on the organization's members to its plan's (TL-418): 20, or 0
// (Clerk: unlimited) on Enterprise. TrackLin.es checks the limit when inviting; this makes Clerk
// refuse the 21st member too, whichever screen they come in through. Members over the cap already
// stay: Clerk only refuses new ones.
func SyncMemberCap(ctx context.Context, db *pgxpool.Pool, clerkOrgs OrgUpdater, org string) error {
	if clerkOrgs == nil {
		return nil
	}
	limit, err := MemberLimitOf(ctx, db, org)
	if err != nil {
		return err
	}
	max := int64(limit)
	if limit < 0 {
		max = 0
	}
	_, err = clerkOrgs.Update(ctx, org, &organization.UpdateParams{MaxAllowedMemberships: &max})
	return err
}

// SyncAllMemberCaps sets every known organization's cap (those with projects or billing), once at
// startup, so organizations from before member limits get theirs.
func SyncAllMemberCaps(ctx context.Context, db *pgxpool.Pool, clerkOrgs OrgUpdater) {
	rows, err := db.Query(ctx, `SELECT org_id FROM org_billing UNION SELECT DISTINCT owner_clerk_id FROM projects WHERE owner_clerk_id LIKE 'org\_%'`)
	if err != nil {
		logs.Warnf("billing: member caps: %v", err)
		return
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		logs.Warnf("billing: member caps: %v", err)
		return
	}
	for _, org := range orgs {
		if ctx.Err() != nil {
			return
		}
		if err := SyncMemberCap(ctx, db, clerkOrgs, org); err != nil {
			logs.Warnf("billing: member cap %s: %v", org, err)
		}
	}
}

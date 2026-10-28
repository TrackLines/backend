package billing

import (
	"context"
	"slices"

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

// Skip leaves Clerk's cap alone for some organizations (CLERK_UNCAPPED_ORGS): an Enterprise org Clerk
// can't make unlimited without its memberships add-on, and that doesn't need it (ours: three accounts).
type Skip struct {
	OrgUpdater
	Orgs []string
}

func (s Skip) Update(ctx context.Context, id string, params *organization.UpdateParams) (*clerk.Organization, error) {
	if slices.Contains(s.Orgs, id) {
		return nil, nil
	}
	return s.OrgUpdater.Update(ctx, id, params)
}

// SyncMemberCap sets Clerk's own cap on the organization's members to its plan's (TL-418): 20, or 0
// (Clerk: unlimited) on Enterprise. TrackLin.es checks the limit when inviting; this makes Clerk
// refuse the 21st member too, whichever screen they come in through. Members over the cap already
// stay: Clerk only refuses new ones.
func SyncMemberCap(ctx context.Context, db *pgxpool.Pool, clerkOrgs OrgUpdater, org string) error {
	return syncMemberCap(ctx, db, clerkOrgs, org, false)
}

// syncMemberCap tells Clerk org's cap and records it; with changedOnly, not when Clerk already has it.
func syncMemberCap(ctx context.Context, db *pgxpool.Pool, clerkOrgs OrgUpdater, org string, changedOnly bool) error {
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
	if changedOnly {
		var same bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM member_caps WHERE org_id = $1 AND cap = $2)`, org, max).Scan(&same); err != nil || same {
			return err
		}
	}
	if _, err := clerkOrgs.Update(ctx, org, &organization.UpdateParams{MaxAllowedMemberships: &max}); err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO member_caps (org_id, cap) VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET cap = EXCLUDED.cap, synced_at = now()`, org, max)
	return err
}

// SyncAllMemberCaps sets every known organization's cap (those with projects or billing) at startup,
// so organizations from before member limits get theirs. Only caps that changed since they were last
// set go to Clerk, so frequent deploys don't spend Clerk's rate limit (TL-427).
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
		if err := syncMemberCap(ctx, db, clerkOrgs, org, true); err != nil {
			logs.Warnf("billing: member cap %s: %v", org, err)
		}
	}
}

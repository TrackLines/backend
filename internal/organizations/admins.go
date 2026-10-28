package organizations

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrNotMember = httpx.Code("not_member", "user is not a member of this organization")
	ErrLastAdmin = httpx.Code("last_admin", "an organization needs at least one admin")
)

// AdminRole is Clerk's role for an organization admin (TL-690); everyone else is MemberRole.
const (
	AdminRole  = "org:admin"
	MemberRole = "org:member"
)

type roleUpdater interface {
	Update(ctx context.Context, params *organizationmembership.UpdateParams) (*clerk.OrganizationMembership, error)
}

// Admins is the org-admin model. Who's an admin is Clerk's org:admin role (TL-690), so SSO and
// Directory Sync role mappings decide it too. org_admins mirrors the role: the admin API writes it
// (with the granting admin, for the org log) and the Clerk webhook writes changes made elsewhere,
// so checks read Postgres and never wait on Clerk.
type Admins struct {
	DB          *pgxpool.Pool
	Memberships membershipLister
	Roles       roleUpdater // changes a member's Clerk role (grant, revoke)
}

// Admin is one admin and how they became one.
type Admin struct {
	UserID    string `json:"user_id"`
	GrantedBy string `json:"granted_by"` // an admin's user id, or clerk:… when Clerk said so (its dashboard, SSO, the first sync)
	CreatedAt string `json:"created_at"`
}

// List returns the org's admins, filling the mirror from Clerk if it has none yet.
func (a Admins) List(ctx context.Context, org string) ([]Admin, error) {
	if err := a.seed(ctx, org); err != nil {
		return nil, err
	}
	rows, err := a.DB.Query(ctx, `SELECT user_clerk_id, granted_by, to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM org_admins WHERE org_id = $1 ORDER BY created_at, user_clerk_id`, org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Admin, error) {
		var ad Admin
		return ad, row.Scan(&ad.UserID, &ad.GrantedBy, &ad.CreatedAt)
	})
}

// IsAdmin reports whether user administers org.
func (a Admins) IsAdmin(ctx context.Context, org, user string) (bool, error) {
	if err := a.seed(ctx, org); err != nil {
		return false, err
	}
	var ok bool
	err := a.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2)`, org, user).Scan(&ok)
	return ok, err
}

// seed fills an empty mirror with the org's Clerk admins: a new org's creator is one, before
// Clerk's webhook arrives. Clerk errors are returned and nothing is stored, so the next call tries again.
func (a Admins) seed(ctx context.Context, org string) error {
	var has bool
	if err := a.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_admins WHERE org_id = $1)`, org).Scan(&has); err != nil || has {
		return err
	}
	return a.pull(ctx, org)
}

// pull adds the org's Clerk admins missing from the mirror.
func (a Admins) pull(ctx context.Context, org string) error {
	limit := int64(100) // ponytail: one page of admins; page through if an org ever has more
	list, err := a.Memberships.List(ctx, &organizationmembership.ListParams{OrganizationID: org, Roles: []string{AdminRole}, ListParams: clerk.ListParams{Limit: &limit}})
	if err != nil {
		return err
	}
	for _, m := range list.OrganizationMemberships {
		if m == nil || m.PublicUserData == nil || m.PublicUserData.UserID == "" || m.Role != AdminRole {
			continue
		}
		if _, err := a.DB.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, $2, 'clerk:admin-role') ON CONFLICT DO NOTHING`, org, m.PublicUserData.UserID); err != nil {
			return err
		}
	}
	return nil
}

// setRole changes user's Clerk role in org; someone who isn't a member is ErrNotMember.
func (a Admins) setRole(ctx context.Context, org, user, role string) error {
	_, err := a.Roles.Update(ctx, &organizationmembership.UpdateParams{OrganizationID: org, UserID: user, Role: &role})
	var api *clerk.APIErrorResponse
	if errors.As(err, &api) && api.HTTPStatusCode == http.StatusNotFound {
		return ErrNotMember
	}
	return err
}

// Grant makes a Clerk member of org an admin, in Clerk and the mirror; by is the granting admin.
func (a Admins) Grant(ctx context.Context, org, user, by string) error {
	if err := a.setRole(ctx, org, user, AdminRole); err != nil {
		return err
	}
	_, err := a.DB.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, org, user, by)
	return err
}

// Revoke makes an admin a member again, refusing to remove the last one. Someone who has
// left the org loses the row anyway.
func (a Admins) Revoke(ctx context.Context, org, user string) error {
	return pgx.BeginFunc(ctx, a.DB, func(tx pgx.Tx) error {
		// lock the org's admin rows so two revokes can't both leave zero
		rows, err := tx.Query(ctx, `SELECT user_clerk_id FROM org_admins WHERE org_id = $1 FOR UPDATE`, org)
		if err != nil {
			return err
		}
		admins, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if !slices.Contains(admins, user) {
			return nil
		}
		if len(admins) == 1 {
			return ErrLastAdmin
		}
		if err := a.setRole(ctx, org, user, MemberRole); err != nil && !errors.Is(err, ErrNotMember) {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2`, org, user)
		return err
	})
}

// SyncToClerk gives the admins recorded before TL-690 Clerk's org:admin role, once each, then
// adds each of those orgs' other Clerk admins to the mirror. Safe to run on every start: rows
// already in Clerk are skipped, and someone who left their org loses the row.
func (a Admins) SyncToClerk(ctx context.Context) error {
	rows, err := a.DB.Query(ctx, `SELECT org_id, user_clerk_id FROM org_admins WHERE NOT in_clerk ORDER BY org_id, created_at`)
	if err != nil {
		return err
	}
	pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var p [2]string
		return p, row.Scan(&p[0], &p[1])
	})
	if err != nil {
		return err
	}
	orgs := map[string]bool{}
	for _, p := range pending {
		org, user := p[0], p[1]
		q := `UPDATE org_admins SET in_clerk = true WHERE org_id = $1 AND user_clerk_id = $2`
		switch err := a.setRole(ctx, org, user, AdminRole); {
		case errors.Is(err, ErrNotMember):
			q = `DELETE FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2`
			logs.Infof("organizations: %s left %s: no longer an admin", user, org)
		case err != nil:
			return err // tried again next start
		default:
			logs.Infof("organizations: %s is org:admin in Clerk for %s", user, org)
		}
		if _, err := a.DB.Exec(ctx, q, org, user); err != nil {
			return err
		}
		orgs[org] = true
	}
	for org := range orgs {
		if err := a.pull(ctx, org); err != nil {
			return err
		}
	}
	return nil
}

// RequireAdmin writes an error and returns false unless the caller is a signed-in admin of their
// active org. Admin changes need a Clerk session: an API key can't promote anyone.
func (a Admins) RequireAdmin(w http.ResponseWriter, r *http.Request) (org, user string, ok bool) {
	org = auth.OrgID(r.Context())
	user, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn || org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("sign_in_as_admin", "sign in as an organization admin to do this"))
		return "", "", false
	}
	isAdmin, err := a.IsAdmin(r.Context(), org, user)
	if err != nil {
		WriteErr(w, r, err)
		return "", "", false
	}
	if !isAdmin {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_only", "only organization admins can do this"))
		return "", "", false
	}
	return org, user, true
}

// IsBoardManager: is user an admin of org, or a leader of the team assigned to board?
func (a Admins) IsBoardManager(ctx context.Context, org, user, board string) (bool, error) {
	admin, err := a.IsAdmin(ctx, org, user)
	if err != nil || admin {
		return admin, err
	}
	var leader bool
	err = a.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM boards b JOIN team_members tm ON tm.team_id = b.team_id
		WHERE b.id::text = $1 AND b.owner_clerk_id = $2 AND tm.user_clerk_id = $3 AND tm.leader)`, board, org, user).Scan(&leader)
	return leader, err
}

// IsProjectManager: is user an admin of org, or a leader of a team with a board in project? Roadmaps
// belong to a project, so these are the people who can change its roadmaps (TL-413).
func (a Admins) IsProjectManager(ctx context.Context, org, user, project string) (bool, error) {
	admin, err := a.IsAdmin(ctx, org, user)
	if err != nil || admin {
		return admin, err
	}
	var leader bool
	err = a.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM boards b JOIN team_members tm ON tm.team_id = b.team_id
		WHERE b.project_id::text = $1 AND b.owner_clerk_id = $2 AND tm.user_clerk_id = $3 AND tm.leader)`, project, org, user).Scan(&leader)
	return leader, err
}

// RequireBoardManager allows an org admin or a leader of the team assigned to a board.
// It scopes leaders to one board and requires a signed-in session for either role.
func (a Admins) RequireBoardManager(w http.ResponseWriter, r *http.Request, boardID string) bool {
	org, user, ok := a.session(w, r)
	if !ok {
		return false
	}
	admin, err := a.IsAdmin(r.Context(), org, user)
	if err != nil {
		WriteErr(w, r, err)
		return false
	}
	if admin {
		return true
	}
	var exists, leader bool
	err = a.DB.QueryRow(r.Context(), `SELECT
		EXISTS (SELECT 1 FROM boards WHERE id::text=$1 AND owner_clerk_id=$2),
		EXISTS (SELECT 1 FROM boards b JOIN team_members tm ON tm.team_id=b.team_id
			WHERE b.id::text=$1 AND b.owner_clerk_id=$2 AND tm.user_clerk_id=$3 AND tm.leader)`, boardID, org, user).Scan(&exists, &leader)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !exists {
		http.NotFound(w, r)
		return false
	}
	if !leader {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_or_leaders_only", "only organization admins or this board's team leaders can do this"))
		return false
	}
	return true
}

// RequireResourceBoardManager resolves a board-scoped resource before checking its manager.
func (a Admins) RequireResourceBoardManager(w http.ResponseWriter, r *http.Request, resource string, id string) bool {
	var boardID string
	var err error
	switch resource {
	case "board":
		boardID = id
	case "column":
		err = a.DB.QueryRow(r.Context(), `SELECT c.board_id::text FROM columns c JOIN boards b ON b.id=c.board_id WHERE c.id::text=$1 AND b.owner_clerk_id=$2`, id, auth.OrgID(r.Context())).Scan(&boardID)
	case "sprint":
		err = a.DB.QueryRow(r.Context(), `SELECT s.board_id::text FROM sprints s JOIN boards b ON b.id=s.board_id WHERE s.id::text=$1 AND b.owner_clerk_id=$2`, id, auth.OrgID(r.Context())).Scan(&boardID)
	case "planned_sprint":
		err = a.DB.QueryRow(r.Context(), `SELECT s.board_id::text FROM planned_sprints s JOIN boards b ON b.id=s.board_id WHERE s.id::text=$1 AND b.owner_clerk_id=$2`, id, auth.OrgID(r.Context())).Scan(&boardID)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return false
	}
	return a.RequireBoardManager(w, r, boardID)
}

func (a Admins) session(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	org := auth.OrgID(r.Context())
	user, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn || org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrSignIn)
		return "", "", false
	}
	return org, user, true
}

// WriteErr maps the admin model's errors to responses.
func WriteErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotMember):
		httpx.Error(w, r, http.StatusUnprocessableEntity, err)
	case errors.Is(err, ErrLastAdmin):
		httpx.Error(w, r, http.StatusConflict, err)
	default:
		logs.Errorf("organizations: admins: %v", err)
		httpx.Error(w, r, http.StatusBadGateway, httpx.Code("check_admins_failed", "could not check organization admins"))
	}
}

// ListAdmins: GET /api/organizations/admins.
func (h System) ListAdmins(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrChooseOrg)
		return
	}
	admins, err := h.admins.List(r.Context(), org)
	if err != nil {
		WriteErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, admins)
}

// GrantAdmin: PUT /api/organizations/admins/{userID}, admins only.
func (h System) GrantAdmin(w http.ResponseWriter, r *http.Request) {
	org, by, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if err := h.admins.Grant(r.Context(), org, r.PathValue("userID"), by); err != nil {
		WriteErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeAdmin: DELETE /api/organizations/admins/{userID}, admins only; never the last one.
func (h System) RevokeAdmin(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if err := h.admins.Revoke(r.Context(), org, r.PathValue("userID")); err != nil {
		WriteErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me: GET /api/organizations/me, the caller's roles in their active org.
func (h System) Me(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	user, _ := auth.UserID(r.Context())
	if org == "" || user == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrChooseOrg)
		return
	}
	admin, err := h.admins.IsAdmin(r.Context(), org, user)
	if err != nil {
		WriteErr(w, r, err)
		return
	}
	rows, err := h.admins.DB.Query(r.Context(), `SELECT tm.team_id::text FROM team_members tm JOIN teams t ON t.id = tm.team_id
		WHERE t.org_id = $1 AND tm.user_clerk_id = $2 AND tm.leader ORDER BY 1`, org, user)
	if err != nil {
		WriteErr(w, r, err)
		return
	}
	leads, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		WriteErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user_id": user, "org_id": org, "admin": admin, "leads": leads})
}

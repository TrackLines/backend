package organizations

import (
	"context"
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrNotMember = errors.New("user is not a member of this organization")
	ErrLastAdmin = errors.New("an organization needs at least one admin")
	ErrNoMembers = errors.New("organization has no members to make admin")
)

type orgGetter interface {
	Get(ctx context.Context, idOrSlug string) (*clerk.Organization, error)
}

// Admins is the org-admin model: who administers each Clerk organization inside TrackLines.
// Orgs and their membership stay in Clerk; this only records which members are admins.
type Admins struct {
	DB          *pgxpool.Pool
	Orgs        orgGetter
	Memberships membershipLister
}

// Admin is one admin and how they became one.
type Admin struct {
	UserID    string `json:"user_id"`
	GrantedBy string `json:"granted_by"` // clerk:creator | clerk:admin-role | clerk:earliest-member | an admin's user id
	CreatedAt string `json:"created_at"`
}

// List returns the org's admins, resolving the first one from Clerk if there are none yet.
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

// IsAdmin reports whether user administers org, resolving the first admin if needed.
func (a Admins) IsAdmin(ctx context.Context, org, user string) (bool, error) {
	if err := a.seed(ctx, org); err != nil {
		return false, err
	}
	var ok bool
	err := a.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2)`, org, user).Scan(&ok)
	return ok, err
}

// seed gives an org with no admins its first one, in order of preference:
//  1. the Clerk user who created the organization, if they're still a member;
//  2. otherwise the longest-standing member with Clerk's org:admin role;
//  3. otherwise the longest-standing member.
//
// An org with no members gets none (ErrNoMembers). Clerk errors are returned and nothing is
// stored, so the next call tries again. Concurrent seeds pick the same person, so they agree.
func (a Admins) seed(ctx context.Context, org string) error {
	var has bool
	if err := a.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_admins WHERE org_id = $1)`, org).Scan(&has); err != nil || has {
		return err
	}
	user, how, err := a.firstAdmin(ctx, org)
	if err != nil {
		return err
	}
	_, err = a.DB.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, org, user, how)
	return err
}

func (a Admins) firstAdmin(ctx context.Context, org string) (string, string, error) {
	o, err := a.Orgs.Get(ctx, org)
	if err != nil {
		return "", "", err
	}
	if o.CreatedBy != "" {
		if ok, err := a.member(ctx, org, o.CreatedBy); err != nil || ok {
			return o.CreatedBy, "clerk:creator", err
		}
	}
	for _, f := range []struct {
		roles []string
		how   string
	}{{[]string{"org:admin"}, "clerk:admin-role"}, {nil, "clerk:earliest-member"}} {
		limit, oldest := int64(1), "+created_at"
		list, err := a.Memberships.List(ctx, &organizationmembership.ListParams{
			OrganizationID: org, Roles: f.roles, OrderBy: &oldest, ListParams: clerk.ListParams{Limit: &limit},
		})
		if err != nil {
			return "", "", err
		}
		for _, m := range list.OrganizationMemberships {
			if m != nil && m.PublicUserData != nil && m.PublicUserData.UserID != "" {
				return m.PublicUserData.UserID, f.how, nil
			}
		}
	}
	return "", "", ErrNoMembers
}

// member reports whether user belongs to the Clerk organization.
func (a Admins) member(ctx context.Context, org, user string) (bool, error) {
	limit := int64(1)
	list, err := a.Memberships.List(ctx, &organizationmembership.ListParams{
		OrganizationID: org, UserIDs: []string{user}, ListParams: clerk.ListParams{Limit: &limit},
	})
	if err != nil {
		return false, err
	}
	for _, m := range list.OrganizationMemberships {
		if m != nil && m.PublicUserData != nil && m.PublicUserData.UserID == user {
			return true, nil
		}
	}
	return false, nil
}

// Grant makes a Clerk member of org an admin; by is the granting admin.
func (a Admins) Grant(ctx context.Context, org, user, by string) error {
	ok, err := a.member(ctx, org, user)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotMember
	}
	_, err = a.DB.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, org, user, by)
	return err
}

// Revoke removes an admin, refusing to remove the last one.
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
		if len(admins) == 1 && admins[0] == user {
			return ErrLastAdmin
		}
		_, err = tx.Exec(ctx, `DELETE FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2`, org, user)
		return err
	})
}

// RequireAdmin writes an error and returns false unless the caller is a signed-in admin of their
// active org. Admin changes need a Clerk session: an API key can't promote anyone.
func (a Admins) RequireAdmin(w http.ResponseWriter, r *http.Request) (org, user string, ok bool) {
	org = auth.OrgID(r.Context())
	user, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn || org == "" {
		http.Error(w, "sign in as an organization admin to do this", http.StatusForbidden)
		return "", "", false
	}
	isAdmin, err := a.IsAdmin(r.Context(), org, user)
	if err != nil {
		WriteErr(w, err)
		return "", "", false
	}
	if !isAdmin {
		http.Error(w, "only organization admins can do this", http.StatusForbidden)
		return "", "", false
	}
	return org, user, true
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
		WriteErr(w, err)
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
		http.Error(w, "only organization admins or this board's team leaders can do this", http.StatusForbidden)
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
		http.Error(w, "sign in to do this", http.StatusForbidden)
		return "", "", false
	}
	return org, user, true
}

// WriteErr maps the admin model's errors to responses.
func WriteErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotMember):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, ErrLastAdmin):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrNoMembers):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		logs.Errorf("organizations: admins: %v", err)
		http.Error(w, "could not check organization admins", http.StatusBadGateway)
	}
}

// ListAdmins: GET /api/organizations/admins.
func (h System) ListAdmins(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if org == "" {
		http.Error(w, "choose an organization first", http.StatusForbidden)
		return
	}
	admins, err := h.admins.List(r.Context(), org)
	if err != nil {
		WriteErr(w, err)
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
		WriteErr(w, err)
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
		WriteErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me: GET /api/organizations/me, the caller's roles in their active org.
func (h System) Me(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	user, _ := auth.UserID(r.Context())
	if org == "" || user == "" {
		http.Error(w, "choose an organization first", http.StatusForbidden)
		return
	}
	admin, err := h.admins.IsAdmin(r.Context(), org, user)
	if err != nil {
		WriteErr(w, err)
		return
	}
	rows, err := h.admins.DB.Query(r.Context(), `SELECT tm.team_id::text FROM team_members tm JOIN teams t ON t.id = tm.team_id
		WHERE t.org_id = $1 AND tm.user_clerk_id = $2 AND tm.leader ORDER BY 1`, org, user)
	if err != nil {
		WriteErr(w, err)
		return
	}
	leads, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		WriteErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user_id": user, "org_id": org, "admin": admin, "leads": leads})
}

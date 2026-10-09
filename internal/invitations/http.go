package invitations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationinvitation"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

const invitationDays int64 = 7

type clerkInvitations interface {
	Create(context.Context, *organizationinvitation.CreateParams) (*clerk.OrganizationInvitation, error)
	Get(context.Context, *organizationinvitation.GetParams) (*clerk.OrganizationInvitation, error)
	Revoke(context.Context, *organizationinvitation.RevokeParams) (*clerk.OrganizationInvitation, error)
}

type membershipLister interface {
	List(context.Context, *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)
}

type userGetter interface {
	Get(context.Context, string) (*clerk.User, error)
}

type System struct {
	db          *pgxpool.Pool
	admins      organizations.Admins
	invitations clerkInvitations
	memberships membershipLister
	users       userGetter
}

func NewSystem(db *pgxpool.Pool, admins organizations.Admins) System {
	cfg := &clerk.ClientConfig{}
	return System{
		db: db, admins: admins,
		invitations: organizationinvitation.NewClient(cfg),
		memberships: organizationmembership.NewClient(cfg),
		users:       user.NewClient(cfg),
	}
}

type Invitation struct {
	ID           string     `json:"id"`
	OrgID        string     `json:"org_id"`
	EmailAddress string     `json:"email_address"`
	Scope        string     `json:"scope"`
	TargetID     *string    `json:"target_id,omitempty"`
	InvitedBy    string     `json:"invited_by"`
	AcceptURL    string     `json:"accept_url,omitempty"`
	Status       string     `json:"status"`
	ExpiresAt    time.Time  `json:"expires_at"`
	AcceptedBy   *string    `json:"accepted_by,omitempty"`
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type inviteInput struct {
	EmailAddress string  `json:"email_address"`
	Scope        string  `json:"scope"` // organization | team | project
	TargetID     *string `json:"target_id"`
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	org, inviter, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in inviteInput
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.EmailAddress = strings.TrimSpace(in.EmailAddress)
	parsed, err := mail.ParseAddress(in.EmailAddress)
	if err != nil || !strings.EqualFold(parsed.Address, in.EmailAddress) {
		http.Error(w, "email_address must be a valid email address", http.StatusBadRequest)
		return
	}
	if in.Scope != "organization" && in.Scope != "team" && in.Scope != "project" {
		http.Error(w, "scope must be organization, team, or project", http.StatusBadRequest)
		return
	}
	if (in.Scope == "organization") != (in.TargetID == nil) {
		http.Error(w, "target_id is required for team and project invitations and omitted for organization invitations", http.StatusBadRequest)
		return
	}
	if in.TargetID != nil {
		exists, err := h.targetExists(r.Context(), org, in.Scope, *in.TargetID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}
	}
	// An existing organization member should be added directly to a team; Clerk cannot invite
	// an existing member, and organization invitations remain the source of truth for membership.
	limit := int64(1)
	users, err := h.memberships.List(r.Context(), &organizationmembership.ListParams{
		OrganizationID: org, EmailAddresses: []string{in.EmailAddress},
		ListParams: clerk.ListParams{Limit: &limit},
	})
	if err != nil {
		writeClerkErr(w, "check organization membership before inviting", err)
		return
	}
	if users != nil && len(users.OrganizationMemberships) > 0 {
		http.Error(w, "user is already an organization member; add them to the team directly", http.StatusConflict)
		return
	}

	metadata, _ := json.Marshal(map[string]string{"tracklines_scope": in.Scope})
	if in.TargetID != nil {
		metadata, _ = json.Marshal(map[string]string{"tracklines_scope": in.Scope, "tracklines_target_id": *in.TargetID})
	}
	expires := invitationDays
	created, err := h.invitations.Create(r.Context(), &organizationinvitation.CreateParams{
		OrganizationID: org,
		EmailAddress:   clerk.String(in.EmailAddress),
		Role:           clerk.String("org:member"),
		InviterUserID:  clerk.String(inviter),
		ExpiresInDays:  &expires,
		PublicMetadata: (*json.RawMessage)(&metadata),
	})
	if err != nil {
		writeClerkErr(w, "create organization invitation", err)
		return
	}
	expiresAt := time.Now().UTC().Add(time.Duration(invitationDays) * 24 * time.Hour)
	if created.ExpiresAt != nil {
		expiresAt = time.UnixMilli(*created.ExpiresAt).UTC()
	}
	var out Invitation
	var target any
	if in.TargetID != nil {
		target = *in.TargetID
	}
	err = h.db.QueryRow(r.Context(), `INSERT INTO invitations
		(org_id,clerk_invitation_id,email_address,scope,target_id,invited_by,accept_url,expires_at)
		VALUES($1,$2,lower($3),$4,$5,$6,$7,$8)
		RETURNING id::text,org_id,email_address,scope,target_id::text,invited_by,accept_url,status,expires_at,accepted_user_clerk_id,accepted_at,created_at`,
		org, created.ID, in.EmailAddress, in.Scope, target, inviter, created.URL, expiresAt).Scan(
		&out.ID, &out.OrgID, &out.EmailAddress, &out.Scope, &out.TargetID, &out.InvitedBy, &out.AcceptURL, &out.Status, &out.ExpiresAt, &out.AcceptedBy, &out.AcceptedAt, &out.CreatedAt)
	if err != nil {
		_, _ = h.invitations.Revoke(r.Context(), &organizationinvitation.RevokeParams{OrganizationID: org, ID: created.ID, RequestingUserID: clerk.String(inviter)})
		http.Error(w, "could not save invitation", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (h System) List(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	rows, err := h.db.Query(r.Context(), `SELECT id::text,org_id,email_address,scope,target_id::text,invited_by,accept_url,status,expires_at,accepted_user_clerk_id,accepted_at,created_at
		FROM invitations WHERE org_id=$1 ORDER BY created_at DESC,id`, org)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := make([]Invitation, 0)
	for rows.Next() {
		var item Invitation
		if err := rows.Scan(&item.ID, &item.OrgID, &item.EmailAddress, &item.Scope, &item.TargetID, &item.InvitedBy, &item.AcceptURL, &item.Status, &item.ExpiresAt, &item.AcceptedBy, &item.AcceptedAt, &item.CreatedAt); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if item.Status == "pending" && time.Now().After(item.ExpiresAt) {
			item.Status = "expired"
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Mine lists pending invitations addressed to the signed-in user's verified email addresses.
// It does not require an active organization because invitees may not be members yet.
func (h System) Mine(w http.ResponseWriter, r *http.Request) {
	if auth.ViaAPIKey(r.Context()) {
		http.Error(w, "invitations require a signed-in session", http.StatusForbidden)
		return
	}
	userID, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	emails, err := h.verifiedEmails(r.Context(), userID)
	if err != nil {
		writeClerkErr(w, "load invitation recipient", err)
		return
	}
	if len(emails) == 0 {
		httpx.JSON(w, http.StatusOK, []Invitation{})
		return
	}
	rows, err := h.db.Query(r.Context(), `SELECT id::text,org_id,email_address,scope,target_id::text,invited_by,accept_url,status,expires_at,accepted_user_clerk_id,accepted_at,created_at
		FROM invitations WHERE lower(email_address)=ANY($1) AND status='pending' AND expires_at>now()
		ORDER BY created_at DESC,id`, emails)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := make([]Invitation, 0)
	for rows.Next() {
		var item Invitation
		if err := rows.Scan(&item.ID, &item.OrgID, &item.EmailAddress, &item.Scope, &item.TargetID, &item.InvitedBy, &item.AcceptURL, &item.Status, &item.ExpiresAt, &item.AcceptedBy, &item.AcceptedAt, &item.CreatedAt); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	item, err := h.get(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, r, err)
		return
	}
	if item.Status == "pending" && time.Now().After(item.ExpiresAt) {
		item.Status = "expired"
	}
	httpx.JSON(w, http.StatusOK, item.Invitation)
}

func (h System) Revoke(w http.ResponseWriter, r *http.Request) {
	org, userID, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	inv, err := h.get(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, r, err)
		return
	}
	if inv.Status != "pending" {
		http.Error(w, "only pending invitations can be revoked", http.StatusConflict)
		return
	}
	if _, err := h.invitations.Revoke(r.Context(), &organizationinvitation.RevokeParams{
		OrganizationID: org, ID: inv.clerkID, RequestingUserID: clerk.String(userID),
	}); err != nil {
		writeClerkErr(w, "revoke organization invitation", err)
		return
	}
	if _, err := h.db.Exec(r.Context(), `UPDATE invitations SET status='revoked' WHERE id=$1 AND org_id=$2 AND status='pending'`, inv.ID, org); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Accept(w http.ResponseWriter, r *http.Request) {
	if auth.ViaAPIKey(r.Context()) {
		http.Error(w, "invitation acceptance requires a signed-in session", http.StatusForbidden)
		return
	}
	userID, signedIn := auth.UserID(r.Context())
	if !signedIn {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	inv, err := h.getByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, r, err)
		return
	}
	if inv.Status == "accepted" {
		if inv.acceptedBy == userID {
			w.WriteHeader(http.StatusNoContent)
		} else {
			http.Error(w, "invitation has already been accepted", http.StatusGone)
		}
		return
	}
	if inv.Status != "pending" || time.Now().After(inv.ExpiresAt) {
		http.Error(w, "invitation is no longer pending", http.StatusGone)
		return
	}
	verified, err := h.hasVerifiedEmail(r.Context(), userID, inv.EmailAddress)
	if err != nil {
		writeClerkErr(w, "load invitation recipient", err)
		return
	}
	if !verified {
		http.Error(w, "signed-in account must have the invited verified email address", http.StatusForbidden)
		return
	}
	clerkInvite, err := h.invitations.Get(r.Context(), &organizationinvitation.GetParams{OrganizationID: inv.OrgID, ID: inv.clerkID})
	if err != nil {
		writeClerkErr(w, "inspect organization invitation", err)
		return
	}
	if clerkInvite.Status == "revoked" || clerkInvite.Status == "expired" {
		http.Error(w, "Clerk invitation is no longer valid", http.StatusGone)
		return
	}
	if clerkInvite.Status != "accepted" {
		http.Error(w, "accept the organization invitation through Clerk before completing this invitation", http.StatusConflict)
		return
	}
	if clerkInvite.OrganizationID != inv.OrgID || !strings.EqualFold(clerkInvite.EmailAddress, inv.EmailAddress) {
		http.Error(w, "invitation details do not match", http.StatusConflict)
		return
	}
	limit := int64(1)
	members, err := h.memberships.List(r.Context(), &organizationmembership.ListParams{
		OrganizationID: inv.OrgID, UserIDs: []string{userID}, ListParams: clerk.ListParams{Limit: &limit},
	})
	if err != nil {
		writeClerkErr(w, "verify organization membership", err)
		return
	}
	if !hasMembership(members, userID) {
		http.Error(w, "organization membership is not active yet", http.StatusConflict)
		return
	}
	err = pgx.BeginFunc(r.Context(), h.db, func(tx pgx.Tx) error {
		locked, err := h.getTx(r.Context(), tx, inv.ID)
		if err != nil {
			return err
		}
		if locked.Status == "accepted" {
			if locked.acceptedBy == userID {
				return nil
			}
			return errInvitationConsumed
		}
		if locked.Status != "pending" || time.Now().After(locked.ExpiresAt) {
			return errInvitationUnavailable
		}
		switch locked.Scope {
		case "team":
			tag, err := tx.Exec(r.Context(), `INSERT INTO team_members(team_id,user_clerk_id) SELECT $1,$2 WHERE EXISTS(SELECT 1 FROM teams WHERE id=$1 AND org_id=$3) ON CONFLICT DO NOTHING`, locked.targetID, userID, locked.OrgID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				var exists bool
				if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM teams WHERE id=$1 AND org_id=$2)`, locked.targetID, locked.OrgID).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					return pgx.ErrNoRows
				}
			}
		case "project":
			tag, err := tx.Exec(r.Context(), `INSERT INTO project_members(project_id,user_clerk_id,invited_by)
				SELECT $1,$2,$3 WHERE EXISTS(SELECT 1 FROM projects WHERE id=$1 AND owner_clerk_id=$4) ON CONFLICT DO NOTHING`, locked.targetID, userID, locked.InvitedBy, locked.OrgID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				var exists bool
				if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM projects WHERE id::text=$1 AND owner_clerk_id=$2)`, locked.targetID, locked.OrgID).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					return pgx.ErrNoRows
				}
			}
		}
		_, err = tx.Exec(r.Context(), `UPDATE invitations SET status='accepted',accepted_user_clerk_id=$2,accepted_at=now() WHERE id=$1`, locked.ID, userID)
		return err
	})
	if errors.Is(err, errInvitationConsumed) {
		http.Error(w, "invitation has already been accepted", http.StatusGone)
		return
	}
	if errors.Is(err, errInvitationUnavailable) || errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invitation is no longer available", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, "could not accept invitation", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type dbInvitation struct {
	Invitation
	clerkID    string
	acceptedBy string
	targetID   string
}

var (
	errInvitationConsumed    = errors.New("invitation already accepted by another user")
	errInvitationUnavailable = errors.New("invitation unavailable")
)

const invitationColumns = `id::text,org_id,email_address,scope,COALESCE(target_id::text,''),invited_by,accept_url,status,expires_at,COALESCE(accepted_user_clerk_id,''),accepted_at,created_at,clerk_invitation_id`

func (h System) get(ctx context.Context, org, id string) (dbInvitation, error) {
	return scanInvitation(h.db.QueryRow(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE id::text=$1 AND org_id=$2`, id, org))
}

func (h System) getByID(ctx context.Context, id string) (dbInvitation, error) {
	return scanInvitation(h.db.QueryRow(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE id::text=$1`, id))
}

func (h System) getTx(ctx context.Context, tx pgx.Tx, id string) (dbInvitation, error) {
	return scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE id::text=$1 FOR UPDATE`, id))
}

type rowScanner interface{ Scan(...any) error }

func scanInvitation(row rowScanner) (dbInvitation, error) {
	var out dbInvitation
	err := row.Scan(&out.ID, &out.OrgID, &out.EmailAddress, &out.Scope, &out.targetID, &out.InvitedBy, &out.AcceptURL, &out.Status, &out.ExpiresAt, &out.acceptedBy, &out.AcceptedAt, &out.CreatedAt, &out.clerkID)
	if out.targetID != "" {
		out.TargetID = &out.targetID
	}
	if out.acceptedBy != "" {
		out.AcceptedBy = &out.acceptedBy
	}
	return out, err
}

func (h System) targetExists(ctx context.Context, org, scope, id string) (bool, error) {
	var exists bool
	var err error
	switch scope {
	case "team":
		err = h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams WHERE id::text=$1 AND org_id=$2)`, id, org).Scan(&exists)
	case "project":
		err = h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id::text=$1 AND owner_clerk_id=$2)`, id, org).Scan(&exists)
	}
	return exists, err
}

func (h System) hasVerifiedEmail(ctx context.Context, userID, email string) (bool, error) {
	emails, err := h.verifiedEmails(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, address := range emails {
		if strings.EqualFold(address, email) {
			return true, nil
		}
	}
	return false, nil
}

func (h System) verifiedEmails(ctx context.Context, userID string) ([]string, error) {
	person, err := h.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	emails := make([]string, 0, len(person.EmailAddresses))
	for _, address := range person.EmailAddresses {
		if address != nil && address.EmailAddress != "" && address.Verification != nil && address.Verification.Status == "verified" {
			emails = append(emails, strings.ToLower(address.EmailAddress))
		}
	}
	return emails, nil
}

func hasMembership(list *clerk.OrganizationMembershipList, userID string) bool {
	if list == nil {
		return false
	}
	for _, item := range list.OrganizationMemberships {
		if item != nil && item.PublicUserData != nil && item.PublicUserData.UserID == userID {
			return true
		}
	}
	return false
}

func writeStoreErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	logs.Errorf("invitations: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeClerkErr(w http.ResponseWriter, action string, err error) {
	logs.Errorf("invitations: %s: %v", action, err)
	if w != nil {
		http.Error(w, "could not "+action, http.StatusBadGateway)
	}
}

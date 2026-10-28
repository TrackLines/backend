// Package clerkhooks keeps stored org data in step with Clerk: when someone leaves an org, is
// deleted, or the org is deleted, their admin and team rows go and their API keys are revoked.
// Access checks still ask Clerk directly (webhooks can be missed); this is cleanup. Joining also
// completes the newcomer's TrackLin.es invitations (TL-518), and a membership's role keeps the
// org_admins mirror in step with Clerk (TL-690).
package clerkhooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/organizations"
)

// Author is who a comment the system writes is by (shown as recorded: it's not a person's id).
const Author = "TrackLin.es"

// unassign frees the open tickets assigned to $user (in projects owned by $org, unless org is
// empty: everywhere) and says why on each, in a comment (TL-608). Done tickets keep their assignee.
// A replay finds nothing still assigned, so it never comments twice.
func unassign(org, user, why string) string {
	scope := ""
	if org != "" {
		scope = ` AND t.project_id IN (SELECT id FROM projects WHERE owner_clerk_id = ` + org + `)`
	}
	return `WITH freed AS (
		UPDATE tickets_all t SET assigned_to = NULL, updated_at = now()
		WHERE t.assigned_to = ` + user + scope + ` AND NOT ` + boards.TicketDoneSQL + `
		RETURNING t.id)
	INSERT INTO ticket_comments (ticket_id, body, author)
	SELECT id, '` + why + `', '` + Author + `' FROM freed`
}

// auditMember queues a membership change for the org log (TL-669; paid plans only): $1 org, $2 user, $3 action.
const auditMember = `SELECT audit_org($1, $3, 'user', $2, NULL, '{}')`

type System struct {
	DB     *pgxpool.Pool
	Secret string // CLERK_WEBHOOK_SECRET (whsec_…); empty = endpoint answers 503
	// ClerkOrgs sets a new organization's member cap (TL-418); nil: left to the startup sync
	ClerkOrgs billing.OrgUpdater
	// Invites completes someone's pending invitations once they've joined the org (TL-518); nil: skipped
	Invites interface {
		CompleteFor(ctx context.Context, org, userID string) (int, error)
	}
	// Forget drops an org's cached member lists when its membership changes (TL-520); nil: left to the TTL
	Forget func(ctx context.Context, org string)
}

func NewSystem(db *pgxpool.Pool, secret string) System { return System{DB: db, Secret: secret} }

type event struct {
	Type string `json:"type"`
	Data struct {
		ID           string `json:"id"` // user.deleted, organization.deleted, organizationInvitation.revoked
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"` // organizationMembership.deleted
		PublicUserData struct {
			UserID string `json:"user_id"`
		} `json:"public_user_data"`
		Role           string `json:"role"`            // organizationMembership.created/updated
		OrganizationID string `json:"organization_id"` // organizationInvitation.revoked
	} `json:"data"`
}

// Webhook: POST /api/webhooks/clerk. Authenticated by Svix signature, not session. Every change is
// a delete/revoke or an insert-if-missing that's safe to repeat, so Svix's at-least-once delivery is fine.
func (s System) Webhook(now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Secret == "" {
			http.Error(w, "clerk webhooks are not configured", http.StatusServiceUnavailable)
			return
		}
		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := Verify(payload, r.Header.Get("svix-id"), r.Header.Get("svix-timestamp"), r.Header.Get("svix-signature"), s.Secret, now()); err != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		var e event
		if err := json.Unmarshal(payload, &e); err != nil {
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		if err := s.apply(r.Context(), e); err != nil {
			logs.Errorf("clerk webhook %s: %v", e.Type, err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable) // Svix retries
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// apply runs one event's cleanup in a transaction. A deleted org's projects, boards, tickets and files
// are kept 30 days (recoverable; nobody can reach them without the org), then Purge removes them for
// good. A leaver's open tickets are unassigned; done ones keep their assignee.
func (s System) apply(ctx context.Context, e event) error {
	if e.Type == "organization.created" { // a new organization starts on Free: up to 20 members (TL-418)
		return billing.SyncMemberCap(ctx, s.DB, s.ClerkOrgs, e.Data.ID)
	}
	if s.Forget != nil && strings.HasPrefix(e.Type, "organizationMembership.") {
		s.Forget(ctx, e.Data.Organization.ID) // pickers and the API key check see the change now
	}
	if e.Type == "organizationMembership.created" || e.Type == "organizationMembership.updated" {
		org, user := e.Data.Organization.ID, e.Data.PublicUserData.UserID
		if org == "" || user == "" {
			return nil
		}
		// the admin mirror follows Clerk's role (TL-690), whoever changed it: the admin API (its row is
		// already there), Clerk's dashboard, or an SSO role mapping. ponytail: trusts delivery order;
		// read the role back from Clerk if a stale event ever undoes a newer change.
		q := `DELETE FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2`
		if e.Data.Role == organizations.AdminRole {
			q = `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, $2, 'clerk:admin-role') ON CONFLICT DO NOTHING`
		}
		if _, err := s.DB.Exec(ctx, q, org, user); err != nil {
			return err
		}
		if e.Type == "organizationMembership.updated" {
			return nil
		}
		// they joined: finish their invitations (TL-518)
		if _, err := s.DB.Exec(ctx, auditMember, org, user, "member.joined"); err != nil { // the org log (TL-669)
			return err
		}
		if s.Invites == nil {
			return nil
		}
		_, err := s.Invites.CompleteFor(ctx, org, user)
		return err
	}
	var stmts []string
	var args []any
	switch e.Type {
	case "organizationMembership.deleted":
		org, user := e.Data.Organization.ID, e.Data.PublicUserData.UserID
		if org == "" || user == "" {
			return nil
		}
		args = []any{org, user}
		stmts = []string{
			`DELETE FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2`,
			`DELETE FROM guide_completions WHERE org_id = $1 AND user_id = $2`,
			`DELETE FROM team_members WHERE user_clerk_id = $2 AND team_id IN (SELECT id FROM teams WHERE org_id = $1)`,
			`UPDATE api_keys SET revoked_at = now() WHERE org_id = $1 AND owner_clerk_id = $2 AND revoked_at IS NULL`,
			unassign("$1", "$2", "Unassigned automatically: the assignee left the organization, so this ticket is free to pick up."),
			`SELECT audit_org($1, 'member.removed', 'user', $2, NULL, '{}')`, // the org log (TL-669)
		}
	case "organizationInvitation.revoked": // revoked in Clerk's dashboard: no longer pending, nor counted (TL-524)
		if e.Data.ID == "" || e.Data.OrganizationID == "" {
			return nil
		}
		args = []any{e.Data.ID, e.Data.OrganizationID}
		stmts = []string{`UPDATE invitations SET status = 'revoked' WHERE clerk_invitation_id = $1 AND org_id = $2 AND status = 'pending'`}
	case "user.deleted":
		if e.Data.ID == "" {
			return nil
		}
		args = []any{e.Data.ID}
		stmts = []string{
			`DELETE FROM org_admins WHERE user_clerk_id = $1`,
			`DELETE FROM guide_completions WHERE user_id = $1`,
			`DELETE FROM team_members WHERE user_clerk_id = $1`,
			`UPDATE api_keys SET revoked_at = now() WHERE owner_clerk_id = $1 AND revoked_at IS NULL`,
			unassign("", "$1", "Unassigned automatically: the assignee’s account was deleted, so this ticket is free to pick up."),
			`DELETE FROM chat_dms WHERE user_clerk_id = $1`, // no more Chat messages to them (TL-289)
			// their personal data goes too (TL-594, UK GDPR erasure); their id stays on tickets and
			// comments they made, which show as an unknown person. Stripe keeps its own billing records.
			`UPDATE org_billing SET billing_email = '' WHERE billing_email <> ''
				AND lower(billing_email) = (SELECT lower(email) FROM users WHERE clerk_id = $1)`,
			`DELETE FROM notifications WHERE user_id = $1`,
			`DELETE FROM push_subscriptions WHERE user_id = $1`,
			`DELETE FROM ticket_mentions WHERE user_id = $1`,
			`DELETE FROM ticket_watch_mutes WHERE user_id = $1`,
			`DELETE FROM time_away WHERE user_id = $1`,
			`DELETE FROM project_members WHERE user_clerk_id = $1`,
			`DELETE FROM invitations WHERE accepted_user_clerk_id = $1`, // they hold the email address
			`DELETE FROM users WHERE clerk_id = $1`,
		}
	case "organization.deleted":
		if e.Data.ID == "" {
			return nil
		}
		args = []any{e.Data.ID}
		stmts = []string{
			`DELETE FROM org_admins WHERE org_id = $1`,
			`DELETE FROM teams WHERE org_id = $1`, // cascades team_members, project_teams; boards.team_id → NULL
			`UPDATE api_keys SET revoked_at = now() WHERE org_id = $1 AND revoked_at IS NULL`,
			`INSERT INTO deleted_orgs (org_id) VALUES ($1) ON CONFLICT DO NOTHING`, // purged after 30 days
		}
	default:
		logs.Debugf("clerk webhook: ignoring %s", e.Type)
		return nil
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

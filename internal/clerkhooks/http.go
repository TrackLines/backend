// Package clerkhooks keeps stored org data in step with Clerk: when someone leaves an org, is
// deleted, or the org is deleted, their admin and team rows go and their API keys are revoked.
// Access checks still ask Clerk directly (webhooks can be missed); this is cleanup.
package clerkhooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/billing"
)

type System struct {
	DB     *pgxpool.Pool
	Secret string // CLERK_WEBHOOK_SECRET (whsec_…); empty = endpoint answers 503
	// ClerkOrgs sets a new organization's member cap (TL-418); nil: left to the startup sync
	ClerkOrgs billing.OrgUpdater
}

func NewSystem(db *pgxpool.Pool, secret string) System { return System{DB: db, Secret: secret} }

type event struct {
	Type string `json:"type"`
	Data struct {
		ID           string `json:"id"` // user.deleted, organization.deleted
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"` // organizationMembership.deleted
		PublicUserData struct {
			UserID string `json:"user_id"`
		} `json:"public_user_data"`
	} `json:"data"`
}

// Webhook: POST /api/webhooks/clerk. Authenticated by Svix signature, not session. Every change is
// a delete/revoke that's safe to repeat, so Svix's at-least-once, unordered delivery is fine.
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
// good. Ticket assignments are kept too.
func (s System) apply(ctx context.Context, e event) error {
	if e.Type == "organization.created" { // a new organization starts on Free: up to 20 members (TL-418)
		return billing.SyncMemberCap(ctx, s.DB, s.ClerkOrgs, e.Data.ID)
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
			`DELETE FROM team_members WHERE user_clerk_id = $2 AND team_id IN (SELECT id FROM teams WHERE org_id = $1)`,
			`UPDATE api_keys SET revoked_at = now() WHERE org_id = $1 AND owner_clerk_id = $2 AND revoked_at IS NULL`,
		}
	case "user.deleted":
		if e.Data.ID == "" {
			return nil
		}
		args = []any{e.Data.ID}
		stmts = []string{
			`DELETE FROM org_admins WHERE user_clerk_id = $1`,
			`DELETE FROM team_members WHERE user_clerk_id = $1`,
			`UPDATE api_keys SET revoked_at = now() WHERE owner_clerk_id = $1 AND revoked_at IS NULL`,
			`DELETE FROM chat_dms WHERE user_clerk_id = $1`, // no more Chat messages to them (TL-289)
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

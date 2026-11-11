package integrations

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/audit"
	"github.com/tracklines/backend/internal/httpx"
)

// ConfigureWebhook stores the secret shown once by flags.gg when an administrator
// creates a company webhook. The empty string disables incoming deliveries.
func (f FlagsSystem) ConfigureWebhook(w http.ResponseWriter, r *http.Request) {
	org, _, ok := f.h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Secret *string `json:"secret"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Secret == nil || len(*in.Secret) > 500 {
		http.Error(w, "invalid webhook secret", http.StatusBadRequest)
		return
	}
	var sealed []byte
	var err error
	if *in.Secret != "" {
		sealed, err = f.h.store.Box.SealWithAAD(*in.Secret, "flags-hook|"+org)
		if err != nil {
			flagsErr(w, r, err)
			return
		}
	}
	tag, err := f.h.store.DB.Exec(r.Context(), `UPDATE org_integrations SET flags_webhook_secret = $2 WHERE org_id = $1 AND provider = 'flags'`, org, sealed)
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		flagsErr(w, r, ErrNotConnected)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"webhook_configured": *in.Secret != "", "webhook_path": "/api/webhooks/flags/" + org})
}

type flagWebhookEvent struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      struct {
		FlagID        string `json:"flag_id"`
		Name          string `json:"name"`
		Ticket        string `json:"ticket_ref"`
		AgentID       string `json:"agent_id"`
		EnvironmentID string `json:"environment_id"`
		Enabled       bool   `json:"enabled"`
		Rollout       int    `json:"rollout"`
	} `json:"data"`
}

func verifyFlagsWebhook(secret, stamp, signature string, body []byte, now time.Time) bool {
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return false
	}
	at := time.Unix(seconds, 0)
	if at.Before(now.Add(-5*time.Minute)) || at.After(now.Add(5*time.Minute)) {
		return false
	}
	if !strings.HasPrefix(signature, "v1=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, "v1="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Webhook authenticates the exact flags.gg payload, scoped to the connection's
// organization. Replay claims, state and activity commit together, so a failed
// transaction remains retryable. Redelivery is acknowledged without processing.
func (f FlagsSystem) Webhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := r.PathValue("org")
	var sealed []byte
	err := f.h.store.DB.QueryRow(ctx, `SELECT flags_webhook_secret FROM org_integrations WHERE org_id = $1 AND provider = 'flags' AND flags_webhook_secret IS NOT NULL`, org).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	secret, err := f.h.store.Box.OpenWithAAD(sealed, "flags-hook|"+org)
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if f.h.now != nil {
		now = f.h.now()
	}
	if !verifyFlagsWebhook(secret, r.Header.Get("X-Flags-Timestamp"), r.Header.Get("X-Flags-Signature"), body, now) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var e flagWebhookEvent
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" || len(e.ID) > 100 || e.ID != r.Header.Get("X-Flags-Event-ID") || e.CreatedAt.IsZero() || e.CreatedAt.After(now.Add(5*time.Minute)) {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	if e.Type != "flag.created" && e.Type != "flag.state_changed" && e.Type != "flag.archived" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if e.Data.Ticket == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if e.Data.FlagID == "" || e.Data.AgentID == "" || e.Data.EnvironmentID == "" || e.Data.Rollout < 0 || e.Data.Rollout > 100 {
		http.Error(w, "invalid flag state", http.StatusBadRequest)
		return
	}
	err = pgx.BeginFunc(ctx, f.h.store.DB, func(tx pgx.Tx) error {
		var id, project string
		// Keys and agents alone are not credentials: constrain every lookup to
		// the organization whose secret authenticated this delivery.
		err := tx.QueryRow(ctx, `SELECT t.id::text, p.id::text FROM tickets t
			JOIN projects p ON p.id = t.project_id
			LEFT JOIN boards b ON b.id = t.board_id
			WHERE p.owner_clerk_id = $1 AND upper(p.key_prefix || '-' || t.number) = upper($2)
			AND t.needs_flag AND COALESCE(t.flag_agent_id, b.flags_agent_id, p.flags_agent_id, '') = $3
			FOR UPDATE OF t`, org, e.Data.Ticket, e.Data.AgentID).Scan(&id, &project)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(e.ID))
		tag, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (provider, body_hash) VALUES ($1, $2) ON CONFLICT DO NOTHING`, "flags:"+org, hash[:])
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		tag, err = tx.Exec(ctx, `INSERT INTO ticket_flag_states (ticket_id, agent_id, environment_id, flag_id, name, enabled, rollout, archived, event_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (ticket_id, agent_id, environment_id) DO UPDATE SET flag_id=EXCLUDED.flag_id, name=EXCLUDED.name,
			 enabled=EXCLUDED.enabled, rollout=EXCLUDED.rollout, archived=EXCLUDED.archived, event_at=EXCLUDED.event_at
			WHERE ticket_flag_states.event_at < EXCLUDED.event_at`, id, e.Data.AgentID, e.Data.EnvironmentID, e.Data.FlagID, e.Data.Name, e.Data.Enabled, e.Data.Rollout, e.Type == "flag.archived", e.CreatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE tickets SET updated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Org: org, Action: e.Type, TargetType: "ticket", TargetID: id, ProjectID: project,
			Data: map[string]any{"provider": "flags", "environment_id": e.Data.EnvironmentID, "agent_id": e.Data.AgentID, "enabled": e.Data.Enabled, "rollout": e.Data.Rollout}})
	})
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

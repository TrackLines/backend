package tickets

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

var ErrUnknownAssignee = errors.New("assignee must be you or an active AI key")

// Assignee is a person or AI agent the owner can assign a ticket to.
type Assignee struct {
	ID    string `json:"id"`    // value stored in tickets.assigned_to
	Label string `json:"label"` // "You" or the AI agent's key name
	Kind  string `json:"kind"`  // "person" or "ai"
}

// Assign lets user (in org) assign a ticket to themself or to one of the org's active AI agent keys,
// or clear assignment.
// ponytail: other org members aren't assignable yet; that needs the member list from Clerk (T-037).
func (s Store) Assign(ctx context.Context, org, user, id string, assignee *string) (*Ticket, error) {
	t, err := boards.ScanTicket(s.DB.QueryRow(ctx, `UPDATE tickets t SET assigned_to = $3, updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2
		AND ($3::text IS NULL OR $3::text = $4 OR EXISTS (
			SELECT 1 FROM api_keys k WHERE k.org_id = $2 AND k.name = $3
			AND k.kind = 'ai' AND k.revoked_at IS NULL
		)) RETURNING `+boards.TicketCols, id, org, assignee, user))
	if err == nil {
		return &t, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, org).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	return nil, ErrUnknownAssignee
}

// Assignees lists user and org's active AI agent keys (server/service keys are excluded).
func (s Store) Assignees(ctx context.Context, org, user string) ([]Assignee, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT name FROM api_keys
		WHERE org_id = $1 AND kind = 'ai' AND revoked_at IS NULL ORDER BY name`, org)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := []Assignee{{ID: user, Label: "You", Kind: "person"}}
	for _, n := range names {
		out = append(out, Assignee{ID: n, Label: n, Kind: "ai"})
	}
	return out, err
}

// Assign: PUT /api/tickets/{id}/assignee {"assignee": "<id>" | null}.
func (h System) Assign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Assignee *string `json:"assignee"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	user, _ := auth.UserID(r.Context())
	t, err := h.store.Assign(r.Context(), auth.OrgID(r.Context()), user, r.PathValue("id"), in.Assignee)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

// Assignees: GET /api/assignees.
func (h System) Assignees(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.Assignees(r.Context(), auth.OrgID(r.Context()), user)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

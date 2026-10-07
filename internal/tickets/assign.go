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

// Assign lets the owner assign to themself or an active AI agent key, or clear assignment.
func (s Store) Assign(ctx context.Context, owner, id string, assignee *string) (*Ticket, error) {
	t, err := boards.ScanTicket(s.DB.QueryRow(ctx, `UPDATE tickets t SET assigned_to = $3, updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2
		AND ($3::text IS NULL OR $3::text = $2 OR EXISTS (
			SELECT 1 FROM api_keys k WHERE k.owner_clerk_id = $2 AND k.name = $3
			AND k.kind = 'ai' AND k.revoked_at IS NULL
		)) RETURNING `+boards.TicketCols, id, owner, assignee))
	if err == nil {
		return &t, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	return nil, ErrUnknownAssignee
}

// Assignees lists the owner and active AI agent keys (server/service keys are excluded).
func (s Store) Assignees(ctx context.Context, owner string) ([]Assignee, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT name FROM api_keys
		WHERE owner_clerk_id = $1 AND kind = 'ai' AND revoked_at IS NULL ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := []Assignee{{ID: owner, Label: "You", Kind: "person"}}
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
	t, err := h.store.Assign(r.Context(), user, r.PathValue("id"), in.Assignee)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

// Assignees: GET /api/assignees.
func (h System) Assignees(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.Assignees(r.Context(), user)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

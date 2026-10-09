package tickets

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// OpenTicket is a ticket that isn't done, without its description: enough to choose what to work on.
type OpenTicket struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Type       string   `json:"type"`
	Priority   string   `json:"priority"`
	Estimate   *string  `json:"estimate"`
	Labels     []string `json:"labels"`
	AssignedTo *string  `json:"assigned_to"`
	Blocked    bool     `json:"blocked"`
	BoardID    *string  `json:"board_id"` // null in the backlog
	BoardName  *string  `json:"board_name"`
	ColumnID   *string  `json:"column_id"`
	ColumnName *string  `json:"column_name"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
}

// OpenFilter narrows OpenTickets: Assignee "unassigned", or an actor id for "mine"; "" = anyone.
type OpenFilter struct {
	Assignee       string
	Unassigned     bool
	ExcludeBlocked bool
}

// OpenTickets lists the project's tickets that aren't done (boards and backlog), in the order work
// is picked: priority, then on a board before in the backlog, then oldest first.
func (s Store) OpenTickets(ctx context.Context, owner, projectID string, f OpenFilter) ([]OpenTicket, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id = $1 AND owner_clerk_id = $2`, projectID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT t.id, t.title, t.type::text, t.priority::text, t.estimate,
			ARRAY(SELECT l.label FROM ticket_labels l WHERE l.ticket_id = t.id ORDER BY lower(l.label)),
			t.assigned_to, `+boards.TicketBlockedSQL+`, t.board_id::text, b.name, t.column_id::text, c.name,
			to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(t.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM tickets t
		LEFT JOIN boards b ON b.id = t.board_id
		LEFT JOIN columns c ON c.id = t.column_id
		WHERE t.project_id = $1 AND NOT `+boards.TicketDoneSQL+`
			AND ($2 = false OR t.assigned_to IS NULL)
			AND ($3 = '' OR t.assigned_to = $3)
			AND ($4 = false OR NOT `+boards.TicketBlockedSQL+`)
		ORDER BY t.priority DESC, t.board_id IS NULL, t.created_at`,
		projectID, f.Unassigned, f.Assignee, f.ExcludeBlocked)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenTicket, error) {
		var t OpenTicket
		return t, row.Scan(&t.ID, &t.Title, &t.Type, &t.Priority, &t.Estimate, &t.Labels, &t.AssignedTo, &t.Blocked,
			&t.BoardID, &t.BoardName, &t.ColumnID, &t.ColumnName, &t.CreatedAt, &t.UpdatedAt)
	})
	if out == nil {
		out = []OpenTicket{}
	}
	return out, err
}

// OpenTickets: GET /api/projects/{id}/open-tickets?assignee=unassigned|me&blocked=exclude
func (h System) OpenTickets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := OpenFilter{ExcludeBlocked: q.Get("blocked") == "exclude"}
	switch q.Get("assignee") {
	case "":
	case "unassigned":
		f.Unassigned = true
	case "me":
		f.Assignee = auth.ActorID(r.Context())
	default:
		http.Error(w, "assignee must be unassigned or me", http.StatusBadRequest)
		return
	}
	if b := q.Get("blocked"); b != "" && b != "exclude" {
		http.Error(w, "blocked must be exclude", http.StatusBadRequest)
		return
	}
	out, err := h.store.OpenTickets(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

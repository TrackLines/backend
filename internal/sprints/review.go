package sprints

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// ReviewTicket is an imported ticket set aside for its board to keep or close before its first
// sprint (TL-541): open, and untouched in the source for over 6 months.
type ReviewTicket struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	Priority  string `json:"priority"`
	UpdatedAt string `json:"updated_at"` // when it last changed in the source (UTC)
	URL       string `json:"url"`        // the original
}

// Review is board's tickets still waiting for a decision, oldest change first.
func (s Store) Review(ctx context.Context, owner, board string) ([]ReviewTicket, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id::text, p.key_prefix || '-' || t.number, t.title, t.type::text, t.priority::text,
			to_char(t.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), coalesce(t.external_url, '')
		FROM import_reviews r JOIN tickets t ON t.id = r.ticket_id JOIN projects p ON p.id = t.project_id
		WHERE r.board_id = $1 AND p.owner_clerk_id = $2 AND t.board_id IS NULL AND NOT `+boards.TicketDoneSQL+`
		ORDER BY t.updated_at, t.number`, board, owner)
	if err != nil {
		return nil, notFound(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReviewTicket, error) {
		var t ReviewTicket
		return t, row.Scan(&t.ID, &t.Key, &t.Title, &t.Type, &t.Priority, &t.UpdatedAt, &t.URL)
	})
	if out == nil {
		out = []ReviewTicket{}
	}
	return out, err
}

// Keep takes ticket off board's review list: it stays in the backlog.
func (s Store) Keep(ctx context.Context, owner, board, ticket string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM import_reviews r USING boards b
		WHERE r.board_id = b.id AND b.id::text = $1 AND b.owner_clerk_id = $2 AND r.ticket_id::text = $3`, board, owner, ticket)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Review: GET /api/boards/{id}/import-review, the imported tickets left to keep or close.
func (h System) Review(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.Review(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Keep: POST /api/boards/{id}/import-review/{ticket}/keep. Board managers only (they start its sprints);
// closing one instead is resolving it as won't do.
func (h System) Keep(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
	if err := h.store.Keep(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), r.PathValue("ticket")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

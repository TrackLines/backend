package tickets

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// Latest lists the owner's newest tickets across every project still in use, newest first: at most
// limit, and at most perProject from any one project, so one busy project can't fill the list.
func (s Store) Latest(ctx context.Context, owner string, limit, perProject int) ([]SearchResult, error) {
	rows, err := s.DB.Query(ctx, `WITH ranked AS (
			SELECT t.id, row_number() OVER (PARTITION BY t.project_id ORDER BY t.created_at DESC, t.id) AS n
			FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $1 AND p.archived_at IS NULL)
		SELECT `+boards.TicketCols+`, p.name, t.board_id, b.name, col.name, `+boards.TicketDoneSQL+`, ''
		FROM ranked r
		JOIN tickets t ON t.id = r.id AND r.n <= $3
		JOIN projects p ON p.id = t.project_id
		LEFT JOIN boards b ON b.id = t.board_id
		LEFT JOIN columns col ON col.id = t.column_id
		ORDER BY t.created_at DESC, t.id
		LIMIT $2`, owner, limit, perProject)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SearchResult, error) {
		var r SearchResult
		return r, row.Scan(append(boards.TicketDest(&r.Ticket), &r.ProjectName, &r.BoardID, &r.BoardName, &r.ColumnName, &r.Done, &r.Snippet)...)
	})
	if out == nil {
		out = []SearchResult{}
	}
	return out, err
}

// Latest: GET /api/tickets/latest?limit=1–20&per_project=1–20 (defaults 5 and 3): the home
// card's "Latest tickets" (the caller's own limits are in /api/me/latest-tickets).
func (h System) Latest(w http.ResponseWriter, r *http.Request) {
	num := func(name string, def int) (int, bool) {
		v := r.URL.Query().Get(name)
		if v == "" {
			return def, true
		}
		n, err := strconv.Atoi(v)
		return n, err == nil && n >= 1 && n <= 20
	}
	limit, ok1 := num("limit", 5)
	per, ok2 := num("per_project", 3)
	if !ok1 || !ok2 {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_latest", "limit and per_project must be 1–20"))
		return
	}
	out, err := h.store.Latest(r.Context(), auth.OrgID(r.Context()), limit, per)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

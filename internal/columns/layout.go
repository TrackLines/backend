package columns

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// The edit board page saves its columns in one go (TL-470): the whole list in order, so adding,
// renaming, WIP limits, deleting and reordering land together or not at all.

// LayoutColumn is one column of a board's layout: an existing one (ID) or a new one (no ID).
type LayoutColumn struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	WIPLimit *int   `json:"wip_limit"` // nil or 0: no limit
}

// ErrBadLayout is a layout that can't be saved; its message says why, for the person editing.
type ErrBadLayout struct{ Why error }

func (e ErrBadLayout) Error() string { return e.Why.Error() }
func (e ErrBadLayout) Unwrap() error { return e.Why }

// ErrColumnInUse: a column left out of the layout still holds tickets in the current sprint.
type ErrColumnInUse struct{ Name string }

func (e ErrColumnInUse) Error() string {
	return fmt.Sprintf("“%s” still has tickets: move them before removing the column", e.Name)
}

const maxColumnName = 80

// Layout sets board's columns to want, in order. The first column (To do: where new tickets go) and
// the last (Done: what makes a ticket done) stay put, so want must start and end with them; WIP
// limits only apply between them. Columns left out are deleted, which, as with Delete, is refused while
// they hold tickets in the board's current sprint (or any, without one).
func (s Store) Layout(ctx context.Context, owner, boardID string, want []LayoutColumn) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var openSprint *string
		if err := tx.QueryRow(ctx, `SELECT (SELECT id FROM sprints WHERE board_id = b.id AND closed_at IS NULL)
			FROM boards b WHERE b.id = $1 AND b.owner_clerk_id = $2 FOR UPDATE`, boardID, owner).Scan(&openSprint); err != nil {
			return notFound(err)
		}
		rows, err := tx.Query(ctx, `SELECT id::text, name FROM columns WHERE board_id = $1 ORDER BY position`, boardID)
		if err != nil {
			return err
		}
		type existing struct{ id, name string }
		have, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (existing, error) {
			var e existing
			return e, r.Scan(&e.id, &e.name)
		})
		if err != nil {
			return err
		}
		if err := checkLayout(want, len(have) > 0, func(i int) string { return have[i].id }, len(have)); err != nil {
			return err
		}
		kept := map[string]bool{}
		for _, c := range want {
			if c.ID != "" {
				kept[c.ID] = true
			}
		}
		// columns left out: deleted, unless they still hold this sprint's tickets
		for _, e := range have {
			if kept[e.id] {
				continue
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE column_id = $1 AND sprint_id IS NOT DISTINCT FROM $2`, e.id, openSprint).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return ErrColumnInUse{Name: e.name}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM columns WHERE id = $1`, e.id); err != nil {
				return err
			}
		}
		// out of the way first, then everything to its place
		if _, err := tx.Exec(ctx, `UPDATE columns SET position = position + 1000000 WHERE board_id = $1`, boardID); err != nil {
			return err
		}
		for i, c := range want {
			limit := c.WIPLimit
			if limit != nil && *limit == 0 || i == 0 || i == len(want)-1 {
				limit = nil // no limit, or a pinned end: To do and Done take none (TL-467)
			}
			if c.ID == "" {
				if _, err := tx.Exec(ctx, `INSERT INTO columns (board_id, name, position, wip_limit) VALUES ($1, $2, $3, $4)`, boardID, c.Name, i, limit); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE columns SET name = $2, position = $3, wip_limit = $4 WHERE id = $1 AND board_id = $5`, c.ID, c.Name, i, limit, boardID); err != nil {
				return err
			}
		}
		return nil
	})
}

// checkLayout: names given and short enough, limits in range, existing ids each at most once and
// all the board's, and the ends pinned to the board's own first and last columns.
func checkLayout(want []LayoutColumn, hasColumns bool, haveID func(int) string, haveN int) error {
	if len(want) < 2 {
		return ErrBadLayout{httpx.Code("layout_too_few", "a board needs at least two columns: To do and Done")}
	}
	ids := map[string]bool{}
	for i := 0; i < haveN; i++ {
		ids[haveID(i)] = true
	}
	seen := map[string]bool{}
	for i := range want {
		c := &want[i]
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" {
			return ErrBadLayout{httpx.Code("layout_unnamed", "every column needs a name")}
		}
		if len([]rune(c.Name)) > maxColumnName {
			return ErrBadLayout{httpx.Code("layout_name_too_long", fmt.Sprintf("column names can be up to %d characters", maxColumnName))}
		}
		if c.WIPLimit != nil && (*c.WIPLimit < 0 || *c.WIPLimit > 999) {
			return ErrBadLayout{httpx.Code("layout_bad_wip", "WIP limits are from 1 to 999, or none")}
		}
		if c.ID != "" {
			if !ids[c.ID] || seen[c.ID] {
				return ErrBadLayout{httpx.Code("layout_stale", "the columns don't match this board: reload and try again")}
			}
			seen[c.ID] = true
		}
	}
	if hasColumns {
		if want[0].ID != haveID(0) {
			return ErrBadLayout{httpx.Code("layout_todo_first", "the first column (To do) stays first")}
		}
		if want[len(want)-1].ID != haveID(haveN-1) {
			return ErrBadLayout{httpx.Code("layout_done_last", "the last column (Done) stays last")}
		}
	}
	return nil
}

// Layout: PUT /api/boards/{boardID}/columns {"columns": [{id?, name, wip_limit}, …]}, board managers
// only. Answers with nothing (204); the page reloads the board.
func (h System) Layout(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("boardID")) {
		return
	}
	var in struct {
		Columns []LayoutColumn `json:"columns"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	err := h.store.Layout(r.Context(), auth.OrgID(r.Context()), r.PathValue("boardID"), in.Columns)
	var bad ErrBadLayout
	var inUse ErrColumnInUse
	switch {
	case errors.As(err, &bad):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.As(err, &inUse):
		httpx.Error(w, r, http.StatusConflict, httpx.Code("column_in_use", err.Error()))
	case err != nil:
		writeErr(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

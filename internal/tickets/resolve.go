package tickets

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

var ErrOnBoard = errors.New("this ticket is on a board: move it to the board's Done column instead")

// Resolve closes a backlog ticket in place: it counts as done everywhere (boards.DoneSQL) and leaves
// the backlog, but keeps its links and stays readable by id. Resolving again is a no-op. Moving it
// onto a board reopens it. Then its parent is completed if this was its last unfinished sub-ticket.
func (s Store) Resolve(ctx context.Context, owner, id string) error {
	return s.ResolveAs(ctx, owner, "", id, ResolutionDone, "")
}

const (
	ResolutionDone   = "done"
	ResolutionWontDo = "wont_do"
)

var ErrBadResolution = errors.New("resolution must be done or wont_do")

// ResolveAs resolves a backlog ticket as done or wont_do (decided against: it stays, readable, so whoever
// raised it can be told). A reason is added as a comment by actor. Resolving again updates the resolution.
func (s Store) ResolveAs(ctx context.Context, owner, actor, id, resolution, reason string) error {
	if resolution != ResolutionDone && resolution != ResolutionWontDo {
		return ErrBadResolution
	}
	tag, err := s.DB.Exec(ctx, `UPDATE tickets t SET resolved_at = COALESCE(t.resolved_at, now()),
			done_at = COALESCE(t.done_at, now()), resolution = $3, updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2 AND t.board_id IS NULL AND t.duplicate_of IS NULL`, id, owner, resolution)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 { // not the owner's ticket, or it's on a board
		var onBoard, duplicate bool
		if err := s.DB.QueryRow(ctx, `SELECT t.board_id IS NOT NULL, t.duplicate_of IS NOT NULL FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, owner).Scan(&onBoard, &duplicate); err != nil {
			return notFound(err)
		}
		if duplicate {
			return ErrIsDuplicate
		}
		return ErrOnBoard
	}
	if reason = strings.TrimSpace(reason); reason != "" && actor != "" {
		label := map[string]string{ResolutionDone: "Resolved", ResolutionWontDo: "Won't do"}[resolution]
		if _, err := s.DB.Exec(ctx, `INSERT INTO ticket_comments (ticket_id, body, author) VALUES ($1, $2, $3)`, id, label+": "+reason, actor); err != nil {
			return err
		}
	}
	return s.completeParent(ctx, owner, id)
}

// completeParent finishes id's parent once every one of its sub-tickets is done or resolved: a
// backlog parent is resolved, a parent on a board moves to that board's Done column. Each of those
// completes its own parent in turn, so it carries up the chain. A parent that's already done, or
// whose children aren't all finished, is left alone.
func (s Store) completeParent(ctx context.Context, owner, id string) error {
	var parent *string
	if err := s.DB.QueryRow(ctx, `SELECT parent_id FROM tickets WHERE id = $1`, id).Scan(&parent); err != nil || parent == nil {
		return ignoreNoRows(err)
	}
	return s.completeIfFinished(ctx, owner, *parent)
}

// completeIfFinished completes parent when all of its sub-tickets are done or resolved. Besides a
// sub-ticket finishing, removing the last unfinished one (delete, detach, re-parent) can finish it;
// adding one never does.
func (s Store) completeIfFinished(ctx context.Context, owner, parentID string) error {
	var parent, board *string
	var children, finished int
	var parentDone bool
	err := s.DB.QueryRow(ctx, `SELECT p.id, p.board_id, `+boards.DoneSQL("p")+`,
			(SELECT count(*) FROM tickets c WHERE c.parent_id = p.id),
			(SELECT count(*) FROM tickets c WHERE c.parent_id = p.id AND `+boards.DoneSQL("c")+`)
		FROM tickets p WHERE p.id = $1`, parentID).
		Scan(&parent, &board, &parentDone, &children, &finished)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (parentDone || children == 0 || finished < children)) {
		return nil
	}
	if err != nil {
		return err
	}
	if board == nil {
		return s.Resolve(ctx, owner, *parent)
	}
	var done string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM columns WHERE board_id = $1 ORDER BY position DESC LIMIT 1`, *board).Scan(&done); err != nil {
		return err
	}
	if err := s.move(ctx, owner, *parent, done, 0, &moveOpts{}); err != nil {
		return err
	}
	return s.completeParent(ctx, owner, *parent)
}

// Resolve: POST /api/tickets/{id}/resolve. 204; 409 for a ticket on a board.
// Resolve: POST /api/tickets/{id}/resolve, optionally {resolution: done|wont_do, reason}.
func (h System) Resolve(w http.ResponseWriter, r *http.Request) {
	in := struct {
		Resolution string `json:"resolution"`
		Reason     string `json:"reason"`
	}{Resolution: ResolutionDone}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &in) { // no body: plain resolve (done)
		return
	}
	if in.Resolution == "" {
		in.Resolution = ResolutionDone
	}
	if len(in.Reason) > 10000 {
		http.Error(w, "reason is too long", http.StatusBadRequest)
		return
	}
	org := auth.OrgID(r.Context())
	before := h.detail(r.Context(), org, r.PathValue("id"))
	if err := h.store.ResolveAs(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.Resolution, in.Reason); err != nil {
		if errors.Is(err, ErrBadResolution) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeErr(w, err)
		return
	}
	if d := h.detail(r.Context(), org, r.PathValue("id")); d != nil && before != nil && (!before.Done || in.Resolution == ResolutionWontDo) {
		if in.Resolution == ResolutionWontDo {
			text := "closed " + ref(d) + " as won't do"
			if reason := strings.TrimSpace(in.Reason); reason != "" {
				text += ": " + reason
			}
			h.tell(r, []string{d.CreatedBy}, "wont_do", text, d)
		} else {
			h.finished(r, d, "resolved")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func ignoreNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

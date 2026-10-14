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

var (
	ErrAlreadyDuplicate = errors.New("ticket is already marked as a duplicate")
	ErrDuplicateTarget  = errors.New("the original ticket must not itself be a duplicate")
	ErrSelfDuplicate    = errors.New("a ticket can't be a duplicate of itself")
)

type DuplicateOptions struct {
	CopyLabels   bool
	CopyComments bool
}

// MarkDuplicate closes id as a duplicate of canonical, optionally copying its labels and
// comments to the original. Both tickets must be in the caller's project.
func (s Store) MarkDuplicate(ctx context.Context, owner, actor, id, canonical string, options DuplicateOptions) error {
	if id == canonical {
		return ErrSelfDuplicate
	}
	var projectID string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT t.project_id FROM tickets t
			JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2 WHERE t.id = $1`, id, owner).
			Scan(&projectID); err != nil {
			return notFound(err)
		}
		var originalProject string
		if err := tx.QueryRow(ctx, `SELECT t.project_id FROM tickets t
			JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2 WHERE t.id = $1`, canonical, owner).
			Scan(&originalProject); err != nil {
			return notFound(err)
		}
		if projectID != originalProject {
			return ErrOtherProject
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ticket-duplicate:' || $1))`, projectID); err != nil {
			return err
		}
		var boardID, current, sprintID, estimate, fromColumn *string
		var alreadyDuplicate bool
		if err := tx.QueryRow(ctx, `SELECT board_id, duplicate_of, sprint_id, estimate, column_id, resolution IS NOT DISTINCT FROM 'duplicate' FROM tickets WHERE id = $1 FOR UPDATE`, id).Scan(&boardID, &current, &sprintID, &estimate, &fromColumn, &alreadyDuplicate); err != nil {
			return notFound(err)
		}
		var originalIsDuplicate bool
		if err := tx.QueryRow(ctx, `SELECT duplicate_of IS NOT NULL OR resolution IS NOT DISTINCT FROM 'duplicate' FROM tickets WHERE id = $1 FOR UPDATE`, canonical).Scan(&originalIsDuplicate); err != nil {
			return notFound(err)
		}
		if originalIsDuplicate {
			return ErrDuplicateTarget
		}
		if current != nil || alreadyDuplicate {
			if current != nil && *current == canonical {
				return nil // retries are idempotent; don't copy comments twice
			}
			return ErrAlreadyDuplicate
		}
		if options.CopyLabels {
			if _, err := tx.Exec(ctx, `INSERT INTO ticket_labels (ticket_id, label)
				SELECT $2, l.label FROM ticket_labels l WHERE l.ticket_id = $1
				ON CONFLICT DO NOTHING`, id, canonical); err != nil {
				return err
			}
		}
		if options.CopyComments {
			var sourceKey string
			if err := tx.QueryRow(ctx, `SELECT p.key_prefix || '-' || t.number FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id = $1`, id).Scan(&sourceKey); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ticket_comments (ticket_id, body, author, created_at)
				SELECT $2, '[Copied from ' || $3 || ' by ' || author || ']' || E'\n\n' || body, $4, created_at
				FROM ticket_comments WHERE ticket_id = $1 ORDER BY created_at, id`, id, canonical, sourceKey, actor); err != nil {
				return err
			}
		}

		var targetColumn *string
		if boardID != nil {
			var doneColumn string
			if err := tx.QueryRow(ctx, `SELECT id FROM columns WHERE board_id = $1 ORDER BY position DESC LIMIT 1`, *boardID).Scan(&doneColumn); err != nil {
				return err
			}
			targetColumn = &doneColumn
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets SET duplicate_of = $2 WHERE duplicate_of = $1`, id, canonical); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tickets SET duplicate_of = $2, resolution = 'duplicate', resolved_at = now(),
			done_at = COALESCE(done_at, now()), column_id = COALESCE($3, column_id),
			position = CASE WHEN $3::uuid IS NULL THEN position ELSE (SELECT COALESCE(max(position) + 1, 0) FROM tickets WHERE column_id = $3) END,
			planned_sprint_id = NULL,
			review_requested_at = NULL, updated_at = now() WHERE id = $1`, id, canonical, targetColumn)
		if err != nil {
			return err
		}
		if fromColumn != nil {
			if err := renumberSource(ctx, tx, id, fromColumn, sprintID); err != nil {
				return err
			}
		}
		// If a closed sprint's recorded scope already included this ticket, remove it from that history.
		if sprintID != nil {
			var closed, scopeKnown bool
			if err := tx.QueryRow(ctx, `SELECT closed_at IS NOT NULL, scope_tickets IS NOT NULL FROM sprints WHERE id = $1`, *sprintID).Scan(&closed, &scopeKnown); err != nil {
				return err
			}
			if closed && scopeKnown {
				if _, err := tx.Exec(ctx, `UPDATE sprints SET scope_tickets = GREATEST(scope_tickets - 1, 0),
					scope_points = CASE WHEN scope_points IS NULL THEN NULL ELSE GREATEST(scope_points - $2, 0) END WHERE id = $1`, *sprintID, boards.Points(estimate)); err != nil {
					return err
				}
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	return s.completeParent(ctx, owner, id)
}

// MarkDuplicate closes POST /api/tickets/{id}/duplicate.
func (h System) MarkDuplicate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DuplicateOf  string `json:"duplicate_of"`
		CopyLabels   bool   `json:"copy_labels"`
		CopyComments bool   `json:"copy_comments"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.DuplicateOf == "" {
		http.Error(w, "duplicate_of is required", http.StatusBadRequest)
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.MarkDuplicate(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.DuplicateOf, DuplicateOptions{CopyLabels: in.CopyLabels, CopyComments: in.CopyComments}); err != nil {
		switch {
		case errors.Is(err, ErrSelfDuplicate), errors.Is(err, ErrOtherProject), errors.Is(err, ErrDuplicateTarget):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrAlreadyDuplicate):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			writeErr(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package tickets

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrNotInReview  = errors.New("this ticket isn't waiting for review")
	ErrPeopleReview = errors.New("only people review agent work")
)

// Review settles an agent's finished ticket waiting for review: approve moves it to Done; otherwise it
// goes back to the board's first column. A comment (by the reviewer) is added when given.
func (s Store) Review(ctx context.Context, owner, actor, id string, approve bool, comment string) error {
	var target string
	err := s.DB.QueryRow(ctx, `SELECT (SELECT c.id FROM columns c WHERE c.board_id = t.board_id ORDER BY c.position `+
		map[bool]string{true: "DESC", false: "ASC"}[approve]+` LIMIT 1)
		FROM tickets t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		WHERE t.id = $1 AND t.review_requested_at IS NOT NULL`, id, owner).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.Get(ctx, owner, id); gerr != nil {
			return gerr
		}
		return ErrNotInReview
	}
	if err != nil {
		return notFound(err)
	}
	if _, err := s.MoveEstimated(ctx, owner, id, target, 0, nil, false, false); err != nil { // a person's move: clears the request
		return err
	}
	if comment = strings.TrimSpace(comment); comment != "" {
		_, err = s.DB.Exec(ctx, `INSERT INTO ticket_comments (ticket_id, body, author) VALUES ($1, $2, $3)`, id, comment, actor)
	}
	return err
}

// ReviewTicket: POST /api/tickets/{id}/review {approve, comment?}. People only (not API keys).
func (h System) ReviewTicket(w http.ResponseWriter, r *http.Request) {
	if auth.ViaAPIKey(r.Context()) {
		http.Error(w, ErrPeopleReview.Error(), http.StatusForbidden)
		return
	}
	var in struct {
		Approve *bool  `json:"approve"`
		Comment string `json:"comment"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Approve == nil {
		http.Error(w, "approve (true or false) is required", http.StatusBadRequest)
		return
	}
	if len(in.Comment) > 10000 {
		http.Error(w, "comment is too long", http.StatusBadRequest)
		return
	}
	err := h.store.Review(r.Context(), auth.OrgID(r.Context()), auth.ActorID(r.Context()), r.PathValue("id"), *in.Approve, in.Comment)
	if err == nil {
		if d := h.detail(r.Context(), auth.OrgID(r.Context()), r.PathValue("id")); d != nil {
			if *in.Approve {
				h.finished(r, d, "approved and finished")
			} else if d.AssignedTo != nil {
				h.tell(r, []string{*d.AssignedTo}, "sent_back", "sent back "+ref(d)+": "+in.Comment, d)
			}
		}
	}
	if errors.Is(err, ErrNotInReview) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

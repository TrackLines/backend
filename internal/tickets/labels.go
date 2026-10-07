package tickets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

const (
	MaxLabels   = 20
	MaxLabelLen = 50
)

var ErrBadLabels = errors.New("invalid labels")

// NormalizeLabels trims, drops blanks and case-insensitive duplicates (first spelling wins), and
// enforces the limits. Shared with the BugFixes endpoint.
func NormalizeLabels(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" || seen[strings.ToLower(l)] {
			continue
		}
		if utf8.RuneCountInString(l) > MaxLabelLen {
			return nil, fmt.Errorf("%w: %q is longer than %d characters", ErrBadLabels, l, MaxLabelLen)
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	if len(out) > MaxLabels {
		return nil, fmt.Errorf("%w: at most %d per ticket", ErrBadLabels, MaxLabels)
	}
	return out, nil
}

// SetLabels replaces ticket id's labels (already normalised); [] clears them.
func (s Store) SetLabels(ctx context.Context, owner, id string, labels []string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2 FOR UPDATE OF t`, id, owner).Scan(&ok); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM ticket_labels WHERE ticket_id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ticket_labels (ticket_id, label) SELECT $1, unnest($2::text[])`, id, labels)
		return err
	})
}

type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"` // tickets using it
}

// ProjectLabels lists the labels in use in the owner's project, most used first (for suggestions).
func (s Store) ProjectLabels(ctx context.Context, owner, projectID string) ([]LabelCount, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id = $1 AND owner_clerk_id = $2`, projectID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT min(l.label), count(*) FROM ticket_labels l JOIN tickets t ON t.id = l.ticket_id
		WHERE t.project_id = $1 GROUP BY lower(l.label) ORDER BY count(*) DESC, lower(min(l.label))`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LabelCount, error) {
		var c LabelCount
		return c, row.Scan(&c.Label, &c.Count)
	})
}

// SetLabels: PUT /api/tickets/{id}/labels {"labels": [...]}.
func (h System) SetLabels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Labels []string `json:"labels"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	labels, err := NormalizeLabels(in.Labels)
	if err != nil {
		writeErr(w, err)
		return
	}
	user, _ := auth.UserID(r.Context())
	if err := h.store.SetLabels(r.Context(), user, r.PathValue("id"), labels); err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string][]string{"labels": labels})
}

// ProjectLabels: GET /api/projects/{id}/labels.
func (h System) ProjectLabels(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.ProjectLabels(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if out == nil {
		out = []LabelCount{}
	}
	httpx.JSON(w, http.StatusOK, out)
}

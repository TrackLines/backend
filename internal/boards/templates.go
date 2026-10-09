package boards

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Template is a starting setup for new boards: columns in order (the last is Done), style and
// estimate scale. Built-ins live in code (IDs "builtin:…"); an org's own are rows in board_templates.
type Template struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Builtin       bool             `json:"builtin"`
	Style         string           `json:"style"`
	EstimateScale string           `json:"estimate_scale"`
	Columns       []TemplateColumn `json:"columns"`
}

type TemplateColumn struct {
	Name     string `json:"name"`
	WIPLimit *int   `json:"wip_limit"`
}

func wip(n int) *int { return &n }

// Builtins are offered to every org. Simple is the default for a new board.
var Builtins = []Template{
	{ID: "builtin:simple", Name: "Simple", Builtin: true, Style: "sprints", EstimateScale: "none",
		Columns: []TemplateColumn{{Name: "To do"}, {Name: "In progress"}, {Name: "Done"}}},
	{ID: "builtin:scrum", Name: "Scrum", Builtin: true, Style: "sprints", EstimateScale: "fibonacci",
		Columns: []TemplateColumn{{Name: "To do"}, {Name: "In progress"}, {Name: "Review"}, {Name: "Done"}}},
	{ID: "builtin:kanban", Name: "Kanban", Builtin: true, Style: "kanban", EstimateScale: "none",
		Columns: []TemplateColumn{{Name: "Ready"}, {Name: "In progress", WIPLimit: wip(3)}, {Name: "Review", WIPLimit: wip(2)}, {Name: "Done"}}},
}

var ErrBadTemplate = errors.New("invalid template")

// valid checks a template's settings; the message says what's wrong.
func (t *Template) valid() error {
	if t.Name = strings.TrimSpace(t.Name); t.Name == "" || len(t.Name) > 80 {
		return errors.New("name is required (up to 80 characters)")
	}
	if t.Style != "sprints" && t.Style != "kanban" {
		return errors.New("style must be sprints or kanban")
	}
	if _, ok := Scales[t.EstimateScale]; !ok {
		return errors.New("estimate_scale must be none, fibonacci, tshirt, powers or linear")
	}
	if len(t.Columns) < 1 || len(t.Columns) > 20 {
		return errors.New("a template needs 1 to 20 columns")
	}
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Name = strings.TrimSpace(c.Name); c.Name == "" || len(c.Name) > 80 {
			return errors.New("every column needs a name (up to 80 characters)")
		}
		if c.WIPLimit != nil && (*c.WIPLimit < 1 || *c.WIPLimit > 999) {
			return errors.New("wip_limit must be between 1 and 999")
		}
	}
	return nil
}

const templateCols = `id::text, name, style, estimate_scale, columns`

func scanTemplate(row pgx.Row) (Template, error) {
	var t Template
	var cols []byte
	if err := row.Scan(&t.ID, &t.Name, &t.Style, &t.EstimateScale, &cols); err != nil {
		return t, notFound(err)
	}
	return t, json.Unmarshal(cols, &t.Columns)
}

// Templates lists the built-ins, then the org's own by name.
func (s Store) Templates(ctx context.Context, org string) ([]Template, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+templateCols+` FROM board_templates WHERE org_id = $1 ORDER BY lower(name)`, org)
	if err != nil {
		return nil, err
	}
	own, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Template, error) { return scanTemplate(row) })
	return append(append([]Template{}, Builtins...), own...), err
}

// Template finds a built-in or one of the org's templates.
func (s Store) Template(ctx context.Context, org, id string) (Template, error) {
	for _, t := range Builtins {
		if t.ID == id {
			return t, nil
		}
	}
	return scanTemplate(s.DB.QueryRow(ctx, `SELECT `+templateCols+` FROM board_templates WHERE id::text = $1 AND org_id = $2`, id, org))
}

// SaveTemplate stores an org template (already validated).
func (s Store) SaveTemplate(ctx context.Context, org string, t Template) (Template, error) {
	cols, _ := json.Marshal(t.Columns)
	return scanTemplate(s.DB.QueryRow(ctx, `INSERT INTO board_templates (org_id, name, style, estimate_scale, columns)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+templateCols, org, t.Name, t.Style, t.EstimateScale, cols))
}

// TemplateFromBoard copies a board's setup: its columns in order with WIP limits, style and scale.
func (s Store) TemplateFromBoard(ctx context.Context, org, boardID string) (Template, error) {
	var t Template
	if err := s.DB.QueryRow(ctx, `SELECT style, estimate_scale FROM boards WHERE id::text = $1 AND owner_clerk_id = $2`, boardID, org).
		Scan(&t.Style, &t.EstimateScale); err != nil {
		return t, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT name, wip_limit FROM columns WHERE board_id::text = $1 ORDER BY position`, boardID)
	if err != nil {
		return t, err
	}
	t.Columns, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (TemplateColumn, error) {
		var c TemplateColumn
		return c, row.Scan(&c.Name, &c.WIPLimit)
	})
	return t, err
}

func (s Store) DeleteTemplate(ctx context.Context, org, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM board_templates WHERE id::text = $1 AND org_id = $2`, id, org)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// CreateFrom inserts a board into the owner's project set up from t. ErrNotFound when the
// project isn't the owner's.
func (s Store) CreateFrom(ctx context.Context, owner, projectID, name, desc string, t Template) (*Board, error) {
	var b Board
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		if b, err = scan(tx.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name, description, style, estimate_scale)
			SELECT id, owner_clerk_id, $3, $4, $5, $6 FROM projects WHERE id = $1 AND owner_clerk_id = $2
			RETURNING `+cols, projectID, owner, name, desc, t.Style, t.EstimateScale)); err != nil {
			return err
		}
		for pos, c := range t.Columns {
			col := Column{Tickets: []Ticket{}}
			if err := tx.QueryRow(ctx, `INSERT INTO columns (board_id, name, position, wip_limit) VALUES ($1, $2, $3, $4)
				RETURNING id, name, position, wip_limit`, b.ID, c.Name, pos, c.WIPLimit).Scan(&col.ID, &col.Name, &col.Position, &col.WIPLimit); err != nil {
				return err
			}
			b.Columns = append(b.Columns, col)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ListTemplates: GET /api/board-templates, the built-ins plus the org's own. Any org member.
func (h System) ListTemplates(w http.ResponseWriter, r *http.Request) {
	ts, err := h.store.Templates(r.Context(), auth.OrgID(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ts)
}

// CreateTemplate: POST /api/board-templates. Org admins only, like other board setup. Send the
// template ({name, style, estimate_scale, columns}) or {name, from_board_id} to copy a board's setup.
func (h System) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Template
		FromBoardID string `json:"from_board_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	t := in.Template
	if in.FromBoardID != "" {
		from, err := h.store.TemplateFromBoard(r.Context(), org, in.FromBoardID)
		if err != nil {
			writeErr(w, err)
			return
		}
		from.Name = t.Name
		t = from
	}
	if err := t.valid(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := h.store.SaveTemplate(r.Context(), org, t)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, saved)
}

// DeleteTemplate: DELETE /api/board-templates/{id}. Org admins only; built-ins can't be deleted.
func (h System) DeleteTemplate(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if strings.HasPrefix(r.PathValue("id"), "builtin:") {
		http.Error(w, "built-in templates can't be deleted", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteTemplate(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

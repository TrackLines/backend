// Package views is saved views (TL-633): a project's backlog and search filters kept under a name.
// The filters are the web app's to read (a JSON object); the backend only keeps them. A view is
// personal, or shared with the project; shared ones are the project's managers' to make and change.
package views

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Managers says who runs a project: its org's admins and its team leaders (as for roadmaps).
type Managers interface {
	IsProjectManager(ctx context.Context, org, user, project string) (bool, error)
}

type View struct {
	ID        string          `json:"id"`
	ProjectID string          `json:"project_id"`
	Name      string          `json:"name"`
	Shared    bool            `json:"shared"`
	Query     json.RawMessage `json:"query"`
	Mine      bool            `json:"mine"`       // the caller made it
	CanEdit   bool            `json:"can_edit"`   // the caller may rename, change or delete it
	CreatedAt string          `json:"created_at"` // UTC
}

const maxQuery = 4096

var (
	errNotFound   = errors.New("not found")
	ErrBadName    = httpx.Code("bad_view_name", "a view's name must be 1 to 80 characters")
	ErrBadQuery   = httpx.Code("bad_view_query", "a view's query must be a JSON object of at most 4 KB")
	ErrShareRight = httpx.Code("view_share_managers", "only organization admins and this project's team leaders can share views or change shared ones")
)

type System struct {
	DB       *pgxpool.Pool
	Managers Managers
}

const cols = `v.id, v.project_id, v.name, v.shared, v.query, v.owner, to_char(v.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func (h System) scan(row pgx.Row, user string, manager bool) (View, error) {
	var v View
	var owner string
	err := row.Scan(&v.ID, &v.ProjectID, &v.Name, &v.Shared, &v.Query, &owner, &v.CreatedAt)
	v.Mine = owner == user
	v.CanEdit = (!v.Shared && v.Mine) || (v.Shared && manager)
	return v, err
}

// caller is who's asking and whether they manage project (in their org; another org's is not found).
func (h System) caller(r *http.Request, project string) (org, user string, manager bool, err error) {
	ctx := r.Context()
	org = auth.OrgID(ctx)
	user, _ = auth.UserID(ctx)
	var ok bool
	if err = h.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id::text = $1 AND owner_clerk_id = $2`, project, org).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = errNotFound
		}
		return
	}
	if h.Managers != nil && user != "" {
		manager, err = h.Managers.IsProjectManager(ctx, org, user, project)
	}
	return
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case httpx.Canceled(err):
	case errors.Is(err, errNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		httpx.Error(w, r, http.StatusInternalServerError, errors.New("internal error"))
	}
}

type input struct {
	Name   *string         `json:"name"`
	Shared *bool           `json:"shared"`
	Query  json.RawMessage `json:"query"`
}

// check cleans in: a name if sent, a query if sent (a JSON object, ≤ 4 KB).
func (in *input) check(w http.ResponseWriter, r *http.Request) bool {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || utf8.RuneCountInString(n) > 80 {
			httpx.Error(w, r, http.StatusBadRequest, ErrBadName)
			return false
		}
		in.Name = &n
	}
	if in.Query != nil {
		var obj map[string]any
		if len(in.Query) > maxQuery || json.Unmarshal(in.Query, &obj) != nil || obj == nil {
			httpx.Error(w, r, http.StatusBadRequest, ErrBadQuery)
			return false
		}
	}
	return true
}

// List: GET /api/projects/{id}/views: the project's shared views and the caller's own, shared first, by name.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	org, user, manager, err := h.caller(r, project)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	rows, err := h.DB.Query(r.Context(), `SELECT `+cols+` FROM saved_views v
		WHERE v.project_id::text = $1 AND v.org_id = $2 AND (v.shared OR v.owner = $3)
		ORDER BY v.shared DESC, lower(v.name), v.created_at`, project, org, user)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (View, error) { return h.scan(row, user, manager) })
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if out == nil {
		out = []View{}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Create: POST /api/projects/{id}/views {name, shared, query} → 201 View. Sharing is for managers.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	org, user, manager, err := h.caller(r, project)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in input
	if !httpx.Decode(w, r, &in) || !in.check(w, r) {
		return
	}
	if in.Name == nil {
		httpx.Error(w, r, http.StatusBadRequest, ErrBadName)
		return
	}
	shared := in.Shared != nil && *in.Shared
	if shared && !manager {
		httpx.Error(w, r, http.StatusForbidden, ErrShareRight)
		return
	}
	if in.Query == nil {
		in.Query = json.RawMessage(`{}`)
	}
	v, err := h.scan(h.DB.QueryRow(r.Context(), `INSERT INTO saved_views AS v (org_id, project_id, owner, name, shared, query)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+cols, org, project, user, *in.Name, shared, in.Query), user, manager)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, v)
}

// load is view id as the caller sees it (in their org, and theirs or shared), with its project's managers.
func (h System) load(r *http.Request) (View, string, bool, error) {
	ctx := r.Context()
	org := auth.OrgID(ctx)
	user, _ := auth.UserID(ctx)
	var project string
	if err := h.DB.QueryRow(ctx, `SELECT project_id::text FROM saved_views WHERE id::text = $1 AND org_id = $2 AND (shared OR owner = $3)`,
		r.PathValue("id"), org, user).Scan(&project); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = errNotFound
		}
		return View{}, "", false, err
	}
	_, _, manager, err := h.caller(r, project)
	if err != nil {
		return View{}, "", false, err
	}
	v, err := h.scan(h.DB.QueryRow(ctx, `SELECT `+cols+` FROM saved_views v WHERE v.id::text = $1`, r.PathValue("id")), user, manager)
	return v, user, manager, err
}

// Update: PATCH /api/views/{id} {name?, shared?, query?} → View. Your own personal view, or a shared
// one if you manage the project; making a view shared (or not) is for managers.
func (h System) Update(w http.ResponseWriter, r *http.Request) {
	v, user, manager, err := h.load(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var in input
	if !httpx.Decode(w, r, &in) || !in.check(w, r) {
		return
	}
	if !v.CanEdit || (in.Shared != nil && *in.Shared != v.Shared && !manager) {
		httpx.Error(w, r, http.StatusForbidden, ErrShareRight)
		return
	}
	out, err := h.scan(h.DB.QueryRow(r.Context(), `UPDATE saved_views v SET name = COALESCE($2, v.name), shared = COALESCE($3, v.shared),
		query = COALESCE($4::jsonb, v.query), updated_at = now() WHERE v.id::text = $1 RETURNING `+cols,
		v.ID, in.Name, in.Shared, nullIfEmpty(in.Query)), user, manager)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Delete: DELETE /api/views/{id} → 204, by whoever may change it.
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	v, _, _, err := h.load(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if !v.CanEdit {
		httpx.Error(w, r, http.StatusForbidden, ErrShareRight)
		return
	}
	if _, err := h.DB.Exec(r.Context(), `DELETE FROM saved_views WHERE id::text = $1`, v.ID); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func nullIfEmpty(q json.RawMessage) *string {
	if q == nil {
		return nil
	}
	s := string(q)
	return &s
}

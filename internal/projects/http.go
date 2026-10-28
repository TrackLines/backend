package projects

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

type System struct {
	db     *pgxpool.Pool
	store  Store
	admins organizations.Admins
}

// NewSystem exposes the project handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool, admins organizations.Admins) System {
	return System{db: db, store: Store{DB: db}, admins: admins}
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadKeyPrefix):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrKeyPrefixTaken):
		httpx.Error(w, r, http.StatusConflict, err)
	case errors.Is(err, ErrLimit):
		httpx.Error(w, r, http.StatusPaymentRequired, httpx.Code("project_limit", "your plan's project limit is reached — upgrade for more"))
	default:
		logs.Errorf("projects: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

type projectInput struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	KeyPrefix   *string `json:"key_prefix"` // update only: omitted = unchanged
}

func (in *projectInput) valid(w http.ResponseWriter, r *http.Request) bool {
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.ErrNameRequired)
		return false
	}
	return true
}

func (h System) List(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	out, err := h.store.List(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if !httpx.Decode(w, r, &in) || !in.valid(w, r) {
		return
	}
	// the org's plan sets its project limit, whoever creates the project (TL-243)
	limit, err := billing.ProjectLimit(r.Context(), h.db, auth.OrgID(r.Context()))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	p, err := h.store.Create(r.Context(), auth.OrgID(r.Context()), in.Name, in.Description, limit)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, p)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	p, err := h.store.Get(r.Context(), r.PathValue("id"), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

func (h System) Update(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if !httpx.Decode(w, r, &in) || !in.valid(w, r) {
		return
	}
	org := auth.OrgID(r.Context())
	// only org admins rename a project or change its ticket keys (TL-427); anyone can edit its description
	var renamed bool
	if err := h.db.QueryRow(r.Context(), `SELECT name <> $3 FROM projects WHERE id::text = $1 AND owner_clerk_id = $2`, r.PathValue("id"), org, in.Name).Scan(&renamed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, r, err)
		return
	}
	if in.KeyPrefix != nil || renamed {
		if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
			return
		}
	}
	err := h.store.Update(r.Context(), r.PathValue("id"), org, in.Name, in.Description)
	if err == nil && in.KeyPrefix != nil {
		err = h.store.SetKeyPrefix(r.Context(), r.PathValue("id"), org, strings.ToUpper(strings.TrimSpace(*in.KeyPrefix)))
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if err := h.store.Delete(r.Context(), r.PathValue("id"), org); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

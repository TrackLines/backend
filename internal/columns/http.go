package columns

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

// NewSystem exposes the column handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool, admins organizations.Admins) System {
	return System{store: Store{DB: db}, admins: admins}
}

type System struct {
	store  Store
	admins organizations.Admins
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrNotEmpty):
		httpx.Error(w, r, http.StatusConflict, httpx.Code("column_not_empty", "move or delete tickets before deleting this column"))
	case errors.Is(err, ErrDoneMoved):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrInvalidOrder):
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_column_order", "invalid column order"))
	default:
		logs.Errorf("columns: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("boardID")) {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.ErrNameRequired)
		return
	}
	org := auth.OrgID(r.Context())
	col, err := h.store.Create(r.Context(), org, r.PathValue("boardID"), in.Name)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, col)
}

// Update renames a column and/or sets its work-in-progress limit (0 removes it).
func (h System) Update(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "column", r.PathValue("id")) {
		return
	}
	var in struct {
		Name     *string `json:"name"`
		WIPLimit *int    `json:"wip_limit"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name == nil && in.WIPLimit == nil {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("column_update_empty", "name or wip_limit is required"))
		return
	}
	if in.Name != nil {
		if *in.Name = strings.TrimSpace(*in.Name); *in.Name == "" {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("name_empty", "name can't be empty"))
			return
		}
	}
	if in.WIPLimit != nil && (*in.WIPLimit < 0 || *in.WIPLimit > 999) {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_wip_limit", "wip_limit must be between 1 and 999, or 0 to remove it"))
		return
	}
	org := auth.OrgID(r.Context())
	var err error
	if in.Name != nil {
		err = h.store.Rename(r.Context(), org, r.PathValue("id"), *in.Name)
	}
	if err == nil && in.WIPLimit != nil {
		err = h.store.SetWIPLimit(r.Context(), org, r.PathValue("id"), *in.WIPLimit)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "column", r.PathValue("id")) {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Delete(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Reorder(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("boardID")) {
		return
	}
	var in struct {
		ColumnIDs []string `json:"column_ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Reorder(r.Context(), org, r.PathValue("boardID"), in.ColumnIDs); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package columns

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// NewSystem exposes the column handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

type System struct{ store Store }

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrNotEmpty):
		http.Error(w, "move or delete tickets before deleting this column", http.StatusConflict)
	case errors.Is(err, ErrInvalidOrder):
		http.Error(w, "invalid column order", http.StatusBadRequest)
	default:
		logs.Errorf("columns: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	org := auth.OrgID(r.Context())
	col, err := h.store.Create(r.Context(), org, r.PathValue("boardID"), in.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, col)
}

func (h System) Rename(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Rename(r.Context(), org, r.PathValue("id"), in.Name); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if err := h.store.Delete(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Reorder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ColumnIDs []string `json:"column_ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Reorder(r.Context(), org, r.PathValue("boardID"), in.ColumnIDs); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package projects

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/httpx"
)

type System struct {
	db    *pgxpool.Pool
	store Store
}

// NewSystem exposes the project handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{db: db, store: Store{DB: db}}
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrLimit):
		http.Error(w, "free plan allows 1 project — upgrade for unlimited", http.StatusPaymentRequired)
	default:
		logs.Errorf("projects: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

type projectInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (in *projectInput) valid(w http.ResponseWriter) bool {
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return false
	}
	return true
}

func (h System) List(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.List(r.Context(), user)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	user, _ := auth.UserID(r.Context())
	limit, err := billing.ProjectLimit(r.Context(), h.db, user)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := h.store.Create(r.Context(), user, in.Name, in.Description, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, p)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	p, err := h.store.Get(r.Context(), r.PathValue("id"), user)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

func (h System) Update(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if !httpx.Decode(w, r, &in) || !in.valid(w) {
		return
	}
	user, _ := auth.UserID(r.Context())
	if err := h.store.Update(r.Context(), r.PathValue("id"), user, in.Name, in.Description); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	if err := h.store.Delete(r.Context(), r.PathValue("id"), user); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

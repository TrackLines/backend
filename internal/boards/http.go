package boards

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

// NewSystem exposes the board handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{db: db, store: Store{DB: db}}
}

type System struct {
	db    *pgxpool.Pool
	store Store
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrLimit):
		http.Error(w, "free plan allows 1 board — upgrade for unlimited", http.StatusPaymentRequired)
	default:
		logs.Errorf("boards: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	user, _ := auth.UserID(r.Context())
	limit, err := billing.BoardLimit(r.Context(), h.db, user)
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := h.store.Create(r.Context(), user, in.Name, in.Description, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, b)
}

func (h System) List(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.ListByOwner(r.Context(), user)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	b, err := h.store.Get(r.Context(), r.PathValue("id"), user)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	if err := h.store.Delete(r.Context(), r.PathValue("id"), user); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

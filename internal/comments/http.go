package comments

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

type System struct{ store Store }

// NewSystem exposes the comment handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System { return System{store: Store{DB: db}} }

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadParent), errors.Is(err, ErrBadBody):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		logs.Errorf("comments: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// List returns ticket {id}'s conversation, oldest first.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.List(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Create posts {"body", "parent_comment_id"?} on ticket {id}; author is the caller (API key name or user).
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body     string  `json:"body"`
		ParentID *string `json:"parent_comment_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	user, _ := auth.UserID(r.Context())
	c, err := h.store.Create(r.Context(), user, auth.ActorID(r.Context()), r.PathValue("id"), in.Body, in.ParentID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}

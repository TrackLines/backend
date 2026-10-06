package bugfixesTickets

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// System exposes the Bugfixes ticket-creation handler.
// Routes are declared in internal/service.go.
type System struct{ store *Store }

func NewSystem(db *pgxpool.Pool) System {
	return System{store: &Store{DB: db}}
}

// Middleware lets `Authorization: Bearer bf_…` act as the named bugfixes agent.
// tl_ keys are handled by apikeys.Middleware; anon/Clerk requests pass through here untouched.
func Middleware(db *pgxpool.Pool) func(http.Handler) http.Handler {
	s := &Store{DB: db}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || !strings.HasPrefix(key, BUGFIXES_KEY_PREFIX) {
				next.ServeHTTP(w, r)
				return
			}
			owner, name, err := s.Owner(r.Context(), key)
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					logs.Errorf("bugfixes-tickets: key lookup: %v", err)
				}
				http.Error(w, "invalid bugfixes api key", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAPIKeyUser(r.Context(), owner, name)))
		})
	}
}

// Create is the Bugfixes ticket-creation endpoint: POST /api/bugfixes/tickets
// Auth: Bearer bf_… key (no Clerk session). See Middleware.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BoardID  string `json:"board_id"`
		ColumnID string `json:"column_id"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		Priority string `json:"priority"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.BoardID == "" || in.ColumnID == "" || in.Title == "" {
		http.Error(w, "board_id, column_id, and title are required", http.StatusBadRequest)
		return
	}
	agent := auth.ActorID(r.Context())
	if agent == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	t, err := h.store.Create(r.Context(), in.BoardID, in.ColumnID, in.Title, in.Body, in.Priority, agent)
	if err != nil {
		if errors.Is(err, ErrBadBoard) {
			http.Error(w, "board not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, ErrBadColumn) {
			http.Error(w, "column not found", http.StatusNotFound)
			return
		}
		logs.Errorf("bugfixes-tickets: create: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusCreated, t)
}

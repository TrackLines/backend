package bugfixesTickets

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/tickets"
)

// System exposes the Bugfixes ticket-creation handler.
// Routes are declared in internal/service.go.
type System struct {
	db      *pgxpool.Pool
	tickets tickets.Store // owner-checked create (project, sprint, position), labels
}

func NewSystem(db *pgxpool.Pool) System {
	return System{db: db, tickets: tickets.Store{DB: db}}
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
			owner, name, kind, err := s.Owner(r.Context(), key)
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					logs.Errorf("bugfixes-tickets: key lookup: %v", err)
				}
				http.Error(w, "invalid bugfixes api key", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAPIKeyUser(r.Context(), owner, name, kind)))
		})
	}
}

// Create is the Bugfixes ticket-creation endpoint: POST /api/bugfixes/tickets
// Auth: Bearer bf_… key (no Clerk session). See Middleware. Creates a bug in the column, as the
// key's agent, on a board the key's owner owns. labels is optional (e.g. ["agent:checkout-api"]).
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BoardID  string   `json:"board_id"`
		ColumnID string   `json:"column_id"`
		Title    string   `json:"title"`
		Body     string   `json:"body"`
		Priority string   `json:"priority"` // low | medium | high | urgent; default medium
		Labels   []string `json:"labels"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Title = strings.TrimSpace(in.Title); in.BoardID == "" || in.ColumnID == "" || in.Title == "" {
		http.Error(w, "board_id, column_id, and title are required", http.StatusBadRequest)
		return
	}
	if !slices.Contains([]string{"", "low", "medium", "high", "urgent"}, in.Priority) {
		http.Error(w, "priority must be low, medium, high or urgent", http.StatusBadRequest)
		return
	}
	labels, err := tickets.NormalizeLabels(in.Labels)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	owner, _ := auth.UserID(r.Context())
	agent := auth.ActorID(r.Context())
	if owner == "" || agent == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// the column must be on the given board, and the board the owner's (CreateAs re-checks ownership)
	var ok bool
	if err := h.db.QueryRow(r.Context(), `SELECT true FROM columns c JOIN boards b ON b.id = c.board_id
		JOIN projects p ON p.id = b.project_id WHERE c.id::text = $1 AND b.id::text = $2 AND p.owner_clerk_id = $3`,
		in.ColumnID, in.BoardID, owner).Scan(&ok); err != nil {
		http.Error(w, "board or column not found", http.StatusNotFound)
		return
	}
	t, err := h.tickets.CreateAs(r.Context(), owner, agent, in.ColumnID, "bug", in.Title, in.Body)
	if err == nil && in.Priority != "" {
		t.Priority = in.Priority
		err = h.tickets.Update(r.Context(), owner, t.ID, "", t.Title, t.Description, in.Priority)
	}
	if err == nil && len(labels) > 0 {
		err = h.tickets.SetLabels(r.Context(), owner, t.ID, labels)
	}
	if errors.Is(err, tickets.ErrNotFound) {
		http.Error(w, "board or column not found", http.StatusNotFound)
		return
	}
	if err != nil {
		logs.Errorf("bugfixes-tickets: create: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusCreated, Ticket{
		ID: t.ID, BoardID: in.BoardID, ColumnID: in.ColumnID, Title: t.Title, Body: t.Description,
		Priority: t.Priority, CreatedBy: t.CreatedBy, CreatedAt: time.Now().UTC().Format(time.RFC3339), Labels: labels,
	})
}

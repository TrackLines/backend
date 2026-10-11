package comments

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// mentions tells everyone body @mentions (not the author) that they were mentioned on the ticket.
func (h System) mentions(r *http.Request, org, ticketID, body string) {
	if h.notify == nil || h.people == nil || !strings.Contains(body, "@") {
		return
	}
	people, err := h.people(r.Context(), org)
	if err != nil {
		logs.Errorf("comments: mention lookup: %v", err)
		return
	}
	to := notifications.Mentioned(body, people)
	if len(to) == 0 {
		return
	}
	var name string
	_ = h.store.DB.QueryRow(r.Context(), `SELECT coalesce(p.key_prefix || '-' || t.number || ': ', '') || t.title
		FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id = $1`, ticketID).Scan(&name)
	notifications.Send(r.Context(), h.notify, org, to, notifications.Event{
		Kind: "mention", Text: "mentioned you on " + name, Actor: auth.ActorID(r.Context()), TicketID: ticketID,
	})
}

type System struct {
	store  Store
	notify notifications.Notifier                                                // nil: nobody is told
	people func(ctx context.Context, org string) ([]notifications.Person, error) // who can be @mentioned
}

// NewSystem exposes the comment handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System { return System{store: Store{DB: db}} }

// WithMentions tells people @mentioned in a comment, looking names up through people.
func (h System) WithMentions(n notifications.Notifier, people func(ctx context.Context, org string) ([]notifications.Person, error)) System {
	h.notify, h.people = n, people
	return h
}

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
	org := auth.OrgID(r.Context())
	out, err := h.store.List(r.Context(), org, r.PathValue("id"))
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
	org := auth.OrgID(r.Context())
	c, err := h.store.Create(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.Body, in.ParentID)
	if err != nil {
		writeErr(w, err)
		return
	}
	h.mentions(r, org, c.TicketID, in.Body)
	httpx.JSON(w, http.StatusCreated, c)
}

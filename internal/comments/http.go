package comments

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// mentions records and tells everyone the comment @mentions (not the author) that they were
// mentioned on the ticket.
func (h System) mentions(r *http.Request, org string, c *Comment, body string) {
	if h.people == nil || !strings.Contains(body, "@") {
		return
	}
	people, err := h.people(r.Context(), org)
	if err != nil {
		logs.Errorf("comments: mention lookup: %v", err)
		return
	}
	var to []string
	for _, id := range notifications.Mentioned(body, people) {
		if id != c.Author {
			to = append(to, id)
		}
	}
	if len(to) == 0 {
		return
	}
	if _, err := h.store.DB.Exec(r.Context(), `INSERT INTO ticket_mentions (org_id, user_id, ticket_id, comment_id, actor)
		SELECT $1, u, $3, $4, $5 FROM unnest($2::text[]) u`, org, to, c.TicketID, c.ID, c.Author); err != nil {
		logs.Errorf("comments: record mentions: %v", err)
	}
	var name string
	_ = h.store.DB.QueryRow(r.Context(), `SELECT coalesce(p.key_prefix || '-' || t.number || ': ', '') || t.title
		FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id = $1`, c.TicketID).Scan(&name)
	notifications.Send(r.Context(), h.notify, org, to, notifications.Event{
		Kind: "mention", Text: "mentioned you on " + name, Actor: c.Author, TicketID: c.TicketID,
	})
}

// Mention is a ticket that mentions the caller, as of its latest mention.
type Mention struct {
	TicketID    string `json:"ticket_id"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	Done        bool   `json:"done"`
	ProjectName string `json:"project_name"`
	Actor       string `json:"actor"`        // who mentioned them last
	CommentID   string `json:"comment_id"`   // "" for mentions from before they were recorded
	MentionedAt string `json:"mentioned_at"` // UTC
}

// Mentions: GET /api/mentions?limit= — tickets that mention the caller, one per ticket, newest
// mention first (limit 1–100, default 10).
func (h System) Mentions(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if r.URL.Query().Get("limit") == "" {
		limit, err = 10, nil
	}
	if err != nil || limit < 1 || limit > 100 {
		http.Error(w, "limit must be 1–100", http.StatusBadRequest)
		return
	}
	// newest first by the full timestamp, then the uuidv7 id, so same-second mentions keep their order
	rows, err := h.store.DB.Query(r.Context(), `SELECT * FROM (
		SELECT DISTINCT ON (m.ticket_id) m.ticket_id::text, p.key_prefix || '-' || t.number, t.title, `+boards.TicketDoneSQL+`, p.name,
			m.actor, coalesce(m.comment_id::text, ''), to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), m.created_at, m.id
		FROM ticket_mentions m JOIN tickets t ON t.id = m.ticket_id JOIN projects p ON p.id = t.project_id
		WHERE m.user_id = $1 AND m.org_id = $2 AND p.owner_clerk_id = $2
		ORDER BY m.ticket_id, m.created_at DESC, m.id DESC) latest
		ORDER BY 9 DESC, 10 DESC LIMIT $3`, user, auth.OrgID(r.Context()), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mention, error) {
		var m Mention
		return m, row.Scan(&m.TicketID, &m.Key, &m.Title, &m.Done, &m.ProjectName, &m.Actor, &m.CommentID, &m.MentionedAt, nil, nil)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
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

// maxBody bounds a comment, as tickets.MaxBody bounds a description (TL-251).
const maxBody = 100_000

// Create posts {"body", "parent_comment_id"?} on ticket {id}; author is the caller (API key name or user).
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body     string  `json:"body"`
		ParentID *string `json:"parent_comment_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if utf8.RuneCountInString(in.Body) > maxBody { // TL-251
		http.Error(w, "comment must be "+strconv.Itoa(maxBody)+" characters or fewer", http.StatusBadRequest)
		return
	}
	org := auth.OrgID(r.Context())
	c, err := h.store.Create(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.Body, in.ParentID)
	if err != nil {
		writeErr(w, err)
		return
	}
	h.mentions(r, org, c, in.Body)
	httpx.JSON(w, http.StatusCreated, c)
}

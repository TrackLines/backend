package comments

import (
	"context"
	"errors"
	"fmt"
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

// mentions records everyone the comment @mentions and tells them (not the author) they were
// mentioned on the ticket; the ticket's other watchers hear there's a new comment (TL-587).
// Authors can mention themselves: it's how you watch a ticket, without a notification.
func (h System) mentions(r *http.Request, org string, c *Comment, body string) {
	var mentioned []string
	if h.people != nil && strings.Contains(body, "@") {
		people, err := h.people(r.Context(), org)
		if err != nil {
			logs.Errorf("comments: mention lookup: %v", err)
		}
		mentioned = notifications.Mentioned(body, people)
	}
	if len(mentioned) > 0 {
		if _, err := h.store.DB.Exec(r.Context(), `INSERT INTO ticket_mentions (org_id, user_id, ticket_id, comment_id, actor)
			SELECT $1, u, $3, $4, $5 FROM unnest($2::text[]) u`, org, mentioned, c.TicketID, c.ID, c.Author); err != nil {
			logs.Errorf("comments: record mentions: %v", err)
		}
	}
	if h.notify == nil {
		return
	}
	var name string
	_ = h.store.DB.QueryRow(r.Context(), `SELECT coalesce(p.key_prefix || '-' || t.number || ': ', '') || t.title
		FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id = $1`, c.TicketID).Scan(&name)
	notifications.Send(r.Context(), h.notify, org, mentioned, notifications.Event{ // never the author: Send's notifiers skip the actor
		Kind: "mention", Text: "mentioned you on " + name, Actor: c.Author, TicketID: c.TicketID,
	})
	notifications.Watch(r.Context(), h.notify, h.store.DB, org, c.TicketID, mentioned, notifications.Event{
		Kind: "watching", Text: "commented on " + name, Actor: c.Author,
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
	Muted       bool   `json:"muted"`        // not watching it any more (TL-587); it stays listed
}

// Mute: PUT /api/mentions/{ticket}/mute stops the caller watching the ticket; DELETE watches it again
// (TL-587). Either way it stays on their Mentions list. 404 for a ticket outside the caller's org.
func (h System) Mute(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	org := auth.OrgID(r.Context())
	var ok bool
	if err := h.store.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id::text = $1 AND p.owner_clerk_id = $2)`, r.PathValue("ticket"), org).Scan(&ok); err != nil {
		writeErr(w, r, err)
		return
	}
	if !ok {
		writeErr(w, r, ErrNotFound)
		return
	}
	query := `INSERT INTO ticket_watch_mutes (org_id, user_id, ticket_id) VALUES ($1, $2, $3::uuid) ON CONFLICT DO NOTHING`
	if r.Method == http.MethodDelete {
		query = `DELETE FROM ticket_watch_mutes WHERE org_id = $1 AND user_id = $2 AND ticket_id = $3::uuid`
	}
	if _, err := h.store.DB.Exec(r.Context(), query, org, user, r.PathValue("ticket")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Mentions: GET /api/mentions?limit=&hide_done= — tickets that mention the caller, one per ticket, newest
// mention first (limit 1–100, default 10). hide_done=true leaves out done and closed tickets.
func (h System) Mentions(w http.ResponseWriter, r *http.Request) {
	limit, ok := intParam(w, r, "limit", 10, 100)
	if !ok {
		return
	}
	out, _, err := h.mentioned(r, limit, 0)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// MentionsPage is one page of the caller's mentions, with how many there are in all.
type MentionsPage struct {
	Mentions []Mention `json:"mentions"`
	Total    int       `json:"total"`
	Page     int       `json:"page"`
	PerPage  int       `json:"per_page"`
}

// PageMentions: GET /api/mentions/page?page=&per_page=&hide_done= — Mentions a page at a time (per_page 1–100, default 25).
func (h System) PageMentions(w http.ResponseWriter, r *http.Request) {
	page, ok := intParam(w, r, "page", 1, 100_000)
	if !ok {
		return
	}
	perPage, ok := intParam(w, r, "per_page", 25, 100)
	if !ok {
		return
	}
	out, total, err := h.mentioned(r, perPage, (page-1)*perPage)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, MentionsPage{Mentions: out, Total: total, Page: page, PerPage: perPage})
}

// intParam reads query parameter name as 1..max, def when absent; a bad value is answered with 400.
func intParam(w http.ResponseWriter, r *http.Request, name string, def, max int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_number_param", fmt.Sprintf("%s must be 1–%d", name, max)))
		return 0, false
	}
	return n, true
}

// mentioned lists the caller's mentioned tickets (latest mention each, newest first) and how many in all.
func (h System) mentioned(r *http.Request, limit, offset int) ([]Mention, int, error) {
	user, _ := auth.UserID(r.Context())
	hideDone := r.URL.Query().Get("hide_done") == "true"
	// newest first by the full timestamp, then the uuidv7 id, so same-second mentions keep their order
	rows, err := h.store.DB.Query(r.Context(), `SELECT *, count(*) OVER () FROM (
		SELECT DISTINCT ON (m.ticket_id) m.ticket_id::text, p.key_prefix || '-' || t.number, t.title, `+boards.TicketDoneSQL+` AS done, p.name,
			m.actor, coalesce(m.comment_id::text, ''), to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), m.created_at, m.id,
			EXISTS (SELECT 1 FROM ticket_watch_mutes w WHERE w.user_id = m.user_id AND w.ticket_id = m.ticket_id) AS muted
		FROM ticket_mentions m JOIN tickets t ON t.id = m.ticket_id JOIN projects p ON p.id = t.project_id
		WHERE m.user_id = $1 AND m.org_id = $2 AND p.owner_clerk_id = $2
		ORDER BY m.ticket_id, m.created_at DESC, m.id DESC) latest
		WHERE NOT ($5 AND done)
		ORDER BY 9 DESC, 10 DESC LIMIT $3 OFFSET $4`, user, auth.OrgID(r.Context()), limit, offset, hideDone)
	if err != nil {
		return nil, 0, err
	}
	total := 0 // ponytail: counted with the rows, so a page past the end reports 0; count separately if that matters
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mention, error) {
		var m Mention
		return m, row.Scan(&m.TicketID, &m.Key, &m.Title, &m.Done, &m.ProjectName, &m.Actor, &m.CommentID, &m.MentionedAt, nil, nil, &m.Muted, &total)
	})
	if out == nil {
		out = []Mention{}
	}
	return out, total, err
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

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadParent), errors.Is(err, ErrBadBody):
		httpx.Error(w, r, http.StatusBadRequest, err)
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
		writeErr(w, r, err)
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
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("comment_too_long", "comment must be "+strconv.Itoa(maxBody)+" characters or fewer"))
		return
	}
	org := auth.OrgID(r.Context())
	c, err := h.store.Create(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in.Body, in.ParentID)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.mentions(r, org, c, in.Body)
	httpx.JSON(w, http.StatusCreated, c)
}

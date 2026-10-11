package notifications

import (
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Notification is one inbox entry.
type Notification struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Text      string  `json:"text"`
	Actor     string  `json:"actor"`
	TicketID  *string `json:"ticket_id"`
	TicketKey *string `json:"ticket_key"`
	BoardID   *string `json:"board_id"`
	CreatedAt string  `json:"created_at"`
	ReadAt    *string `json:"read_at"`
}

type System struct{ store Store }

func NewSystem(s Store) System { return System{store: s} }

func (h System) me(w http.ResponseWriter, r *http.Request) (org, user string, ok bool) {
	user, _ = auth.UserID(r.Context())
	org = auth.OrgID(r.Context())
	if user == "" || org == "" {
		http.Error(w, "sign in to an organization", http.StatusForbidden)
		return "", "", false
	}
	return org, user, true
}

// List: GET /api/notifications?page=1&per_page=20 — unread first, then newest.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.me(w, r)
	if !ok {
		return
	}
	page, per := 1, 20
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 && v <= 100 {
		per = v
	}
	rows, err := h.store.DB.Query(r.Context(), `SELECT n.id, n.kind, n.text, n.actor, n.ticket_id,
			(SELECT p.key_prefix || '-' || t.number FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id = n.ticket_id),
			n.board_id, to_char(n.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			to_char(n.read_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM notifications n WHERE n.user_id = $1 AND n.org_id = $2
		ORDER BY n.read_at IS NULL DESC, n.created_at DESC, n.id DESC LIMIT $3 OFFSET $4`, user, org, per, (page-1)*per)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) {
		var n Notification
		return n, row.Scan(&n.ID, &n.Kind, &n.Text, &n.Actor, &n.TicketID, &n.TicketKey, &n.BoardID, &n.CreatedAt, &n.ReadAt)
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []Notification{}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// UnreadCount: GET /api/notifications/unread-count → {"count": n}.
func (h System) UnreadCount(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.me(w, r)
	if !ok {
		return
	}
	var n int
	if err := h.store.DB.QueryRow(r.Context(), `SELECT count(*) FROM notifications WHERE user_id = $1 AND org_id = $2 AND read_at IS NULL`, user, org).Scan(&n); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"count": n})
}

// Read: POST /api/notifications/{id}/read (only your own).
func (h System) Read(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.me(w, r)
	if !ok {
		return
	}
	tag, err := h.store.DB.Exec(r.Context(), `UPDATE notifications SET read_at = COALESCE(read_at, now())
		WHERE id::text = $1 AND user_id = $2 AND org_id = $3`, r.PathValue("id"), user, org)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReadAll: POST /api/notifications/read-all.
func (h System) ReadAll(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.me(w, r)
	if !ok {
		return
	}
	if _, err := h.store.DB.Exec(r.Context(), `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND org_id = $2 AND read_at IS NULL`, user, org); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Dismiss: DELETE /api/notifications/{id} removes one of your notifications.
func (h System) Dismiss(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.me(w, r)
	if !ok {
		return
	}
	tag, err := h.store.DB.Exec(r.Context(), `DELETE FROM notifications WHERE id::text = $1 AND user_id = $2 AND org_id = $3`, r.PathValue("id"), user, org)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ClearRead: DELETE /api/notifications/read removes the ones you've read.
func (h System) ClearRead(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.me(w, r)
	if !ok {
		return
	}
	if _, err := h.store.DB.Exec(r.Context(), `DELETE FROM notifications WHERE user_id = $1 AND org_id = $2 AND read_at IS NOT NULL`, user, org); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package notifications

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
	Link      *string `json:"link,omitempty"` // where a notice that isn't about a ticket or board opens (TL-424)
	CreatedAt string  `json:"created_at"`
	ReadAt    *string `json:"read_at"`
}

type System struct {
	store Store
	push  *Push
}

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
	rows, err := h.store.DB.Query(r.Context(), `SELECT n.id, n.kind, n.text, n.actor, n.ticket_id, n.link,
			(SELECT p.key_prefix || '-' || t.number FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id = n.ticket_id),
			n.board_id, to_char(n.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			to_char(n.read_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM notifications n WHERE n.user_id = $1 AND n.org_id IN ($2, '')
		ORDER BY n.read_at IS NULL DESC, n.created_at DESC, n.id DESC LIMIT $3 OFFSET $4`, user, org, per, (page-1)*per)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) {
		var n Notification
		return n, row.Scan(&n.ID, &n.Kind, &n.Text, &n.Actor, &n.TicketID, &n.Link, &n.TicketKey, &n.BoardID, &n.CreatedAt, &n.ReadAt)
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
	if err := h.store.DB.QueryRow(r.Context(), `SELECT count(*) FROM notifications WHERE user_id = $1 AND org_id IN ($2, '') AND read_at IS NULL`, user, org).Scan(&n); err != nil {
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
		WHERE id::text = $1 AND user_id = $2 AND org_id IN ($3, '')`, r.PathValue("id"), user, org)
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
	if _, err := h.store.DB.Exec(r.Context(), `UPDATE notifications SET read_at = now() WHERE user_id = $1 AND org_id IN ($2, '') AND read_at IS NULL`, user, org); err != nil {
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
	tag, err := h.store.DB.Exec(r.Context(), `DELETE FROM notifications WHERE id::text = $1 AND user_id = $2 AND org_id IN ($3, '')`, r.PathValue("id"), user, org)
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
	if _, err := h.store.DB.Exec(r.Context(), `DELETE FROM notifications WHERE user_id = $1 AND org_id IN ($2, '') AND read_at IS NOT NULL`, user, org); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// WithPush adds the Web Push endpoints' backing (nil or not configured: push reports disabled).
func (h System) WithPush(p *Push) System { h.push = p; return h }

// pushHosts are the browsers' push services. The server sends to a subscription's endpoint, so only
// these are accepted: anything else would let a browser make the server call any address.
var pushHosts = []string{"fcm.googleapis.com", ".push.services.mozilla.com", ".push.apple.com", ".notify.windows.com"}

// ValidEndpoint: an https URL on a known push service.
func ValidEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || len(raw) > 2048 || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range pushHosts {
		if host == h || (strings.HasPrefix(h, ".") && strings.HasSuffix(host, h)) {
			return true
		}
	}
	return false
}

// PushKey: GET /api/push → {enabled, public_key}: whether this server sends Web Push, and the key
// browsers subscribe with.
func (h System) PushKey(w http.ResponseWriter, r *http.Request) {
	if !h.push.Enabled() {
		httpx.JSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"enabled": true, "public_key": h.push.Config.PublicKey})
}

type pushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// person is the signed-in person (not an API key): devices belong to people.
func (h System) person(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, _ := auth.UserID(r.Context())
	if user == "" || auth.ViaAPIKey(r.Context()) {
		http.Error(w, "sign in to turn on notifications for a device", http.StatusForbidden)
		return "", false
	}
	return user, true
}

// Subscribe: POST /api/push/subscriptions {endpoint, keys: {p256dh, auth}} (the browser's PushSubscription).
// The same browser subscribing again (or a new person on it) takes the row over.
func (h System) Subscribe(w http.ResponseWriter, r *http.Request) {
	user, ok := h.person(w, r)
	if !ok {
		return
	}
	if !h.push.Enabled() {
		http.Error(w, "push notifications aren't set up on this server", http.StatusServiceUnavailable)
		return
	}
	var in pushSubscription
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !ValidEndpoint(in.Endpoint) || in.Keys.P256dh == "" || in.Keys.Auth == "" || len(in.Keys.P256dh) > 200 || len(in.Keys.Auth) > 100 {
		http.Error(w, "not a browser push subscription", http.StatusBadRequest)
		return
	}
	if _, err := h.store.DB.Exec(r.Context(), `INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES ($1, $2, $3, $4)
		ON CONFLICT (endpoint) DO UPDATE SET p256dh = $3, auth = $4 WHERE push_subscriptions.user_id = $1`, user, in.Endpoint, in.Keys.P256dh, in.Keys.Auth); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Unsubscribe: DELETE /api/push/subscriptions {endpoint}: this device stops getting pushes.
func (h System) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	user, ok := h.person(w, r)
	if !ok {
		return
	}
	var in pushSubscription
	if !httpx.Decode(w, r, &in) {
		return
	}
	if _, err := h.store.DB.Exec(r.Context(), `DELETE FROM push_subscriptions WHERE endpoint = $1 AND user_id = $2`, in.Endpoint, user); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

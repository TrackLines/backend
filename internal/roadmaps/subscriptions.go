package roadmaps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/email"
	"github.com/tracklines/backend/internal/httpx"
)

// Email subscriptions to a public roadmap's updates (TL-902). Only public roadmaps: anyone can read
// those, so anyone may subscribe; login_only and team roadmaps have the inbox and push for their members.

var ErrInvalidEmail = httpx.Code("invalid_email", "enter one email address")

// Mailer sends email; email.Sender in the server, a fake in tests.
type Mailer interface {
	On() bool
	Send(ctx context.Context, msgs ...email.Message) error
}

// Subscriptions is what the subscription endpoints need; without it (or while On says no) they answer 404.
type Subscriptions struct {
	Mail Mailer
	On   func() bool // the flags.gg "roadmap-subscriptions" flag
	Site string      // the web app, e.g. https://tracklin.es: where the links in emails go
	API  string      // the public API, e.g. https://api.tracklin.es: the one-click unsubscribe address
}

func (h System) WithSubscriptions(s Subscriptions) System { h.subs = s; return h }

func (h System) subscribable() bool {
	return h.subs.Mail != nil && h.subs.Mail.On() && h.subs.Site != "" && h.subs.On != nil && h.subs.On()
}

// normalEmail: one plain address ("a@b.c", no display name), lower-cased; "" when it isn't one.
func normalEmail(in string) string {
	in = strings.TrimSpace(in)
	a, err := mail.ParseAddress(in)
	if err != nil || a.Address != in || len(in) > 254 || !strings.Contains(in[strings.LastIndex(in, "@")+1:], ".") {
		return ""
	}
	return strings.ToLower(in)
}

// Subscribe saves an unconfirmed subscription and returns the token to email, or "" when nothing should
// be sent: the address is already confirmed, or was sent a confirmation in the last 10 minutes.
func (s Store) Subscribe(ctx context.Context, roadmapID, address string) (string, error) {
	// ponytail: unconfirmed addresses are dropped here after a week, not by the retention job; move it
	// to automation if this table ever gets big.
	if _, err := s.DB.Exec(ctx, `DELETE FROM roadmap_subscriptions WHERE verified_at IS NULL AND sent_at < now() - interval '7 days'`); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	var id string
	err := s.DB.QueryRow(ctx, `INSERT INTO roadmap_subscriptions (roadmap_id, email, token) VALUES ($1, $2, $3)
		ON CONFLICT (roadmap_id, email) DO UPDATE SET token = EXCLUDED.token, sent_at = now()
		WHERE roadmap_subscriptions.verified_at IS NULL AND roadmap_subscriptions.sent_at < now() - interval '10 minutes'
		RETURNING id::text`, roadmapID, address, token).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return token, err
}

// ConfirmSubscription turns a subscription on and returns its roadmap's id; ErrNotFound for an unknown token.
func (s Store) ConfirmSubscription(ctx context.Context, token string) (string, error) {
	var roadmapID string
	err := s.DB.QueryRow(ctx, `UPDATE roadmap_subscriptions SET verified_at = COALESCE(verified_at, now())
		WHERE token = $1 RETURNING roadmap_id::text`, token).Scan(&roadmapID)
	return roadmapID, notFound(err)
}

func (s Store) Unsubscribe(ctx context.Context, token string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM roadmap_subscriptions WHERE token = $1`, token)
	return err
}

// Subscribe takes an email address for a public roadmap and sends it a confirmation link. The answer
// is the same whether or not the address was already subscribed, so it can't be used to find out.
func (h System) Subscribe(w http.ResponseWriter, r *http.Request) {
	if !h.subscribable() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	address := normalEmail(in.Email)
	if address == "" {
		httpx.Error(w, r, http.StatusBadRequest, ErrInvalidEmail)
		return
	}
	rm, err := h.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if rm.Visibility != Public {
		http.Error(w, "not found", http.StatusNotFound) // as if it didn't exist: its members have the inbox
		return
	}
	token, err := h.store.Subscribe(r.Context(), rm.ID, address)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if token != "" {
		link := h.subs.link(rm.ID, "confirm", token)
		title := html.EscapeString(rm.Title)
		err := h.subs.Mail.Send(r.Context(), email.Message{
			To:      address,
			Subject: "Confirm your subscription to " + rm.Title,
			Text:    fmt.Sprintf("Confirm that you want updates to the roadmap %q by email:\n\n%s\n\nIf you didn't ask for this, ignore this email and you won't hear from us again.", rm.Title, link),
			HTML: fmt.Sprintf(`<p>Confirm that you want updates to the roadmap <strong>%s</strong> by email:</p><p><a href="%s">Confirm subscription</a></p><p>If you didn't ask for this, ignore this email and you won't hear from us again.</p>`,
				title, html.EscapeString(link)),
		})
		if err != nil {
			logs.Errorf("roadmaps: confirmation email: %v", err)
			http.Error(w, "the confirmation email could not be sent: try again shortly", http.StatusBadGateway)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// ConfirmSubscription: the link in the confirmation email (through the website). Not behind the flag
// check alone: a link already sent keeps working while email is on.
func (h System) ConfirmSubscription(w http.ResponseWriter, r *http.Request) {
	token, ok := h.token(w, r)
	if !ok {
		return
	}
	roadmapID, err := h.store.ConfirmSubscription(r.Context(), token)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"roadmap_id": roadmapID})
}

// Unsubscribe removes the subscription a token belongs to: from the website's unsubscribe page, or a
// mail client's one-click unsubscribe (RFC 8058: a POST with the token in the query). Always 204, so
// a second click isn't an error. It works with the feature off: nobody is ever stuck subscribed.
func (h System) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	token, ok := h.token(w, r)
	if !ok {
		return
	}
	if err := h.store.Unsubscribe(r.Context(), token); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// token reads ?token= or a JSON {"token"} body; without one it answers 400.
func (h System) token(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := r.URL.Query().Get("token")
	if token == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			Token string `json:"token"`
		}
		if !httpx.Decode(w, r, &in) {
			return "", false
		}
		token = in.Token
	}
	if token == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("token_required", "token is required"))
		return "", false
	}
	return token, true
}

// link is the website page that confirms or unsubscribes: /r/<roadmap>/subscription?<action>=<token>.
func (s Subscriptions) link(roadmapID, action, token string) string {
	return s.Site + "/r/" + roadmapID + "/subscription?" + action + "=" + url.QueryEscape(token)
}

// notifySubscribers emails an update to the roadmap's confirmed subscribers, while it is public.
func (h System) notifySubscribers(ctx context.Context, rm *Roadmap, u *Update) error {
	if !h.subscribable() || rm.Visibility != Public {
		return nil
	}
	rows, err := h.store.DB.Query(ctx, `SELECT email, token FROM roadmap_subscriptions WHERE roadmap_id = $1 AND verified_at IS NOT NULL`, rm.ID)
	if err != nil {
		return err
	}
	health := map[string]string{HealthOnTrack: "On track", HealthAtRisk: "At risk", HealthOffTrack: "Off track"}[u.Health]
	page := h.subs.Site + "/r/" + rm.ID
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (email.Message, error) {
		var to, token string
		err := row.Scan(&to, &token)
		leave := h.subs.link(rm.ID, "unsubscribe", token)
		m := email.Message{
			To:      to,
			Subject: rm.Title + ": " + health,
			Text:    fmt.Sprintf("%s: %s\n\n%s\n\nSee the roadmap: %s\n\nUnsubscribe: %s", rm.Title, health, u.Note, page, leave),
			HTML: fmt.Sprintf(`<p><strong>%s</strong>: %s</p><p style="white-space:pre-wrap">%s</p><p><a href="%s">See the roadmap</a></p><p style="color:#666;font-size:12px">You asked for this roadmap's updates by email. <a href="%s">Unsubscribe</a></p>`,
				html.EscapeString(rm.Title), health, html.EscapeString(u.Note), html.EscapeString(page), html.EscapeString(leave)),
		}
		if h.subs.API != "" { // mail clients' own "Unsubscribe" button
			m.Headers = map[string]string{
				"List-Unsubscribe":      "<" + h.subs.API + "/api/roadmap-subscriptions/unsubscribe?token=" + url.QueryEscape(token) + ">",
				"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
			}
		}
		return m, err
	})
	if err != nil || len(msgs) == 0 {
		return err
	}
	return h.subs.Mail.Send(ctx, msgs...)
}

// notifyInBackground sends after the response: the publisher shouldn't wait on the email provider.
func (h System) notifyInBackground(ctx context.Context, rm *Roadmap, u *Update) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	go func() {
		defer cancel()
		if err := h.notifySubscribers(ctx, rm, u); err != nil {
			logs.Errorf("roadmaps: update emails for %s: %v", rm.ID, err)
		}
	}()
}

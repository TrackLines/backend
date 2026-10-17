package intake

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

type System struct {
	store  Store
	notify notifications.Notifier // announces promoted requests to the project's spaces (TL-288); nil: none
}

// WithNotifier announces a request promoted into the backlog ("added SHOP-12 from the request form").
func (h System) WithNotifier(n notifications.Notifier) System { h.notify = n; return h }

func NewSystem(db *pgxpool.Pool) System { return System{store: Store{DB: db}} }

func writeErr(w http.ResponseWriter, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrClosed):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrTooMany):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, ErrDecided):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// sender is the caller's address for rate limits (httpx.ClientIP: the hop our edge wrote, not one
// the client can make up). Only a hash of it is stored.
func sender(r *http.Request) string { return httpx.RateKey(r) }

// Form: GET /api/intake/{token} (public) → {"open": true}, or 404 when the link is closed or unknown.
// It says nothing else about the project.
func (h System) Form(w http.ResponseWriter, r *http.Request) {
	if !h.store.Open(r.Context(), r.PathValue("token")) {
		http.Error(w, ErrClosed.Error(), http.StatusNotFound)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"open": true})
}

// Submit: POST /api/intake/{token} (public) {type, title, details?, contact?} → 202. A filled-in
// "website" field (hidden from people) marks a bot: it gets the same 202 and nothing is kept.
func (h System) Submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in struct {
		Type    string `json:"type"`
		Title   string `json:"title"`
		Details string `json:"details"`
		Contact string `json:"contact"`
		Website string `json:"website"` // honeypot
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Website != "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := h.store.Submit(r.Context(), r.PathValue("token"), sender(r), in.Type, in.Title, in.Details, in.Contact); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type settings struct {
	Open  bool    `json:"open"`
	Token *string `json:"token"` // the link is /request/{token}
}

// Settings: GET /api/projects/{id}/intake (org members).
func (h System) Settings(w http.ResponseWriter, r *http.Request) {
	token, err := h.store.Settings(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, settings{Open: token != nil, Token: token})
}

// SetOpen: PUT /api/projects/{id}/intake {open, rotate?} (org members): rotate gives a new link, the old one stops working.
func (h System) SetOpen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Open   bool `json:"open"`
		Rotate bool `json:"rotate"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	token, err := h.store.SetOpen(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), in.Open, in.Rotate)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, settings{Open: token != nil, Token: token})
}

// List: GET /api/projects/{id}/intake/submissions?status=pending|promoted|dismissed (org members).
func (h System) List(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	if status != "pending" && status != "promoted" && status != "dismissed" {
		http.Error(w, "status must be pending, promoted or dismissed", http.StatusBadRequest)
		return
	}
	out, err := h.store.List(r.Context(), auth.OrgID(r.Context()), r.PathValue("id"), status, 100)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Promote: POST /api/intake-submissions/{id}/promote → the submission with its new backlog ticket.
func (h System) Promote(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.Promote(r.Context(), auth.OrgID(r.Context()), auth.ActorID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if out.TicketID != nil {
		key := out.Title
		if out.TicketKey != nil {
			key = *out.TicketKey + ": " + out.Title
		}
		notifications.Announce(r.Context(), h.notify, auth.OrgID(r.Context()), notifications.Event{
			Kind: "created", Text: "added " + key + " to the backlog from the request form", Actor: auth.ActorID(r.Context()), TicketID: *out.TicketID,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Dismiss: POST /api/intake-submissions/{id}/dismiss.
func (h System) Dismiss(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Dismiss(r.Context(), auth.OrgID(r.Context()), auth.ActorID(r.Context()), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

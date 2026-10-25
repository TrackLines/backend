package webhooks

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/safehttp"
	"github.com/tracklines/backend/internal/secrets"
)

// Admins says whether user administers org.
type Admins interface {
	IsAdmin(ctx context.Context, org, user string) (bool, error)
}

// System is /api/org/webhooks: org admins (signed in, or their API key), behind the "webhooks" flag.
type System struct {
	store    Store
	admins   Admins
	on       func() bool                              // the flag: off, every route is a 404
	validURL func(ctx context.Context, u string) bool // tests: to point endpoints at a local receiver
}

func NewSystem(db *pgxpool.Pool, box secrets.Box, a Admins, on func() bool) System {
	return System{store: Store{DB: db, Box: box}, admins: a, on: on, validURL: ValidURL}
}

// ValidURL: https, a host, no credentials, and not a private address where the host resolves now.
// Delivery checks again on every connection (safehttp), so DNS changed later can't get round it.
func ValidURL(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(raw) > 2000 {
		return false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		return safehttp.PublicAddress(ip)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil {
		return true // not resolvable now: delivery decides
	}
	for _, ip := range ips {
		if !safehttp.PublicAddress(ip) {
			return false
		}
	}
	return true
}

func validEvents(events []string) bool {
	if len(events) == 0 {
		return false
	}
	for _, e := range events {
		if !slices.Contains(Events, e) {
			return false
		}
	}
	return true
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case httpx.Canceled(err):
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrLimit):
		httpx.Error(w, r, http.StatusConflict, err)
	case errors.Is(err, secrets.ErrNoKey):
		http.Error(w, "webhooks are unavailable: the server has no secret key", http.StatusServiceUnavailable)
	default:
		logs.Errorf("webhooks: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// gate: the flag, then org admins; paid also needs Pro or Enterprise (402).
func (h System) gate(w http.ResponseWriter, r *http.Request, paid bool) (org string, ok bool) {
	if h.on == nil || !h.on() {
		http.NotFound(w, r)
		return "", false
	}
	ctx := r.Context()
	org = auth.OrgID(ctx)
	user, signedIn := auth.UserID(ctx)
	admin := false
	if signedIn && org != "" {
		var err error
		if admin, err = h.admins.IsAdmin(ctx, org, user); err != nil {
			writeErr(w, r, err)
			return "", false
		}
	}
	if !admin {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_only", "only organization admins can do this"))
		return "", false
	}
	if paid {
		plan, err := billing.PlanOf(ctx, h.store.DB, org)
		if err != nil {
			writeErr(w, r, err)
			return "", false
		}
		if plan == billing.Free {
			httpx.Error(w, r, http.StatusPaymentRequired, ErrPlan)
			return "", false
		}
	}
	return org, true
}

// List: GET /api/org/webhooks → the org's endpoints (any plan, so a downgraded org can still tidy up).
func (h System) List(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, false)
	if !ok {
		return
	}
	out, err := h.store.List(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Create: POST /api/org/webhooks {url, events} → 201 {endpoint, secret}; the secret is shown only here.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, true)
	if !ok {
		return
	}
	var in struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !h.validURL(r.Context(), in.URL) {
		httpx.Error(w, r, http.StatusBadRequest, ErrBadURL)
		return
	}
	if !validEvents(in.Events) {
		httpx.Error(w, r, http.StatusBadRequest, ErrBadEvents)
		return
	}
	user, _ := auth.UserID(r.Context())
	e, secret, err := h.store.Create(r.Context(), org, user, in.URL, slices.Compact(slices.Sorted(slices.Values(in.Events))))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"endpoint": e, "secret": secret})
}

// Update: PATCH /api/org/webhooks/{id} with any of {url, events, enabled}.
func (h System) Update(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, false)
	if !ok {
		return
	}
	var in struct {
		URL     *string  `json:"url"`
		Events  []string `json:"events"`
		Enabled *bool    `json:"enabled"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.URL != nil && !h.validURL(r.Context(), *in.URL) {
		httpx.Error(w, r, http.StatusBadRequest, ErrBadURL)
		return
	}
	if in.Events != nil {
		if !validEvents(in.Events) {
			httpx.Error(w, r, http.StatusBadRequest, ErrBadEvents)
			return
		}
		in.Events = slices.Compact(slices.Sorted(slices.Values(in.Events)))
	}
	e, err := h.store.Patch(r.Context(), org, r.PathValue("id"), in.URL, in.Events, in.Enabled)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

// Rotate: POST /api/org/webhooks/{id}/secret → {secret}, the new one, shown only here.
func (h System) Rotate(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, false)
	if !ok {
		return
	}
	secret, err := h.store.Rotate(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"secret": secret})
}

// Delete: DELETE /api/org/webhooks/{id}, with its deliveries.
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, false)
	if !ok {
		return
	}
	if err := h.store.Delete(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Test: POST /api/org/webhooks/{id}/test → 202; a ping goes out with the next deliveries.
func (h System) Test(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, true)
	if !ok {
		return
	}
	if err := h.store.Ping(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// Deliveries: GET /api/org/webhooks/{id}/deliveries?page=N → {deliveries, more}, newest first.
func (h System) Deliveries(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, false)
	if !ok {
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100_000 {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_page", "page must be 1 or more"))
			return
		}
		page = n
	}
	out, more, err := h.store.Deliveries(r.Context(), org, r.PathValue("id"), page)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"deliveries": out, "more": more})
}

// Redeliver: POST /api/org/webhooks/{id}/deliveries/{delivery}/redeliver → 202.
func (h System) Redeliver(w http.ResponseWriter, r *http.Request) {
	org, ok := h.gate(w, r, true)
	if !ok {
		return
	}
	if err := h.store.Redeliver(r.Context(), org, r.PathValue("id"), r.PathValue("delivery")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

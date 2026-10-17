package integrations

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

// CalendarSystem is Google Calendar's settings (TL-405): org members see whether it's connected;
// org admins connect, check and disconnect it.
type CalendarSystem struct {
	DB      *pgxpool.Pool
	Admins  organizations.Admins
	Google  *GoogleCalendar // nil: the server has no service account
	Enabled func() bool     // the flags.gg flag; nil or false: off (404)
	Sync    *CalendarSync   // syncs after connecting and on Sync now; nil: hourly only
}

func (h CalendarSystem) on(w http.ResponseWriter, r *http.Request) bool {
	if h.Enabled == nil || !h.Enabled() {
		http.NotFound(w, r)
		return false
	}
	return true
}

// Get: GET /api/integrations/google-calendar. Whether the server offers it, whether the org is
// connected, and what an admin pastes into Google (our client ID and scope). The admin email is for
// admins only.
func (h CalendarSystem) Get(w http.ResponseWriter, r *http.Request) {
	if !h.on(w, r) {
		return
	}
	org := auth.OrgID(r.Context())
	if org == "" {
		http.Error(w, "sign in to an organization", http.StatusForbidden)
		return
	}
	out := map[string]any{"provider": GoogleCalendarProvider, "available": h.Google != nil, "connected": false, "scope": calendarScope}
	if h.Google != nil {
		out["client_id"] = h.Google.ClientID()
	}
	c, err := getCalendar(r.Context(), h.DB, org)
	if errors.Is(err, errNoCalendar) {
		httpx.JSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		logs.Errorf("google calendar: get: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if user, ok := auth.UserID(r.Context()); !ok || auth.ViaAPIKey(r.Context()) {
		c.AdminEmail = ""
	} else if admin, err := h.Admins.IsAdmin(r.Context(), org, user); err != nil || !admin {
		c.AdminEmail = ""
	}
	out["connected"] = true
	out["connection"] = c
	httpx.JSON(w, http.StatusOK, out)
}

// Connect: PUT /api/integrations/google-calendar {"domain", "admin_email", "holidays_calendar"?}.
// Org admins only. It checks Google lets us in before saving, so a saved connection works.
func (h CalendarSystem) Connect(w http.ResponseWriter, r *http.Request) {
	if !h.on(w, r) {
		return
	}
	org, user, ok := h.Admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if h.Google == nil {
		http.Error(w, "Google Calendar isn't set up on this server", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		Domain           string `json:"domain"`
		AdminEmail       string `json:"admin_email"`
		HolidaysCalendar string `json:"holidays_calendar"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	domain, adminEmail, holidays, err := calendarInput(in.Domain, in.AdminEmail, in.HolidaysCalendar)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !h.check(w, r, adminEmail, holidays) {
		return
	}
	if _, err := h.DB.Exec(r.Context(), `INSERT INTO calendar_connections (org_id, domain, admin_email, holidays_calendar, connected_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id) DO UPDATE SET domain = EXCLUDED.domain, admin_email = EXCLUDED.admin_email,
			holidays_calendar = EXCLUDED.holidays_calendar, connected_by = EXCLUDED.connected_by`,
		org, domain, adminEmail, holidays, user); err != nil {
		logs.Errorf("google calendar: connect: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.Sync != nil { // a first sync now, so people show as away without waiting an hour
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
			defer cancel()
			if err := h.Sync.Org(ctx, org); err != nil {
				logs.Warnf("google calendar: first sync %s: %v", org, err)
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

// SyncNow: POST /api/integrations/google-calendar/sync. Org admins: sync now rather than within the
// hour, and see how it went (the connection, with last_sync_at and last_sync_error).
func (h CalendarSystem) SyncNow(w http.ResponseWriter, r *http.Request) {
	if !h.on(w, r) {
		return
	}
	org, _, ok := h.Admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if h.Sync == nil || h.Google == nil {
		http.Error(w, "Google Calendar isn't set up on this server", http.StatusServiceUnavailable)
		return
	}
	err := h.Sync.Org(r.Context(), org)
	if errors.Is(err, errNoCalendar) {
		http.Error(w, "Google Calendar isn't connected", http.StatusNotFound)
		return
	}
	c, gerr := getCalendar(r.Context(), h.DB, org)
	if gerr != nil {
		logs.Errorf("google calendar: sync: %v", gerr)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, c) // the outcome is in last_sync_error
}

// Test: POST /api/integrations/google-calendar/test. Org admins: is access still granted?
func (h CalendarSystem) Test(w http.ResponseWriter, r *http.Request) {
	if !h.on(w, r) {
		return
	}
	org, _, ok := h.Admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if h.Google == nil {
		http.Error(w, "Google Calendar isn't set up on this server", http.StatusServiceUnavailable)
		return
	}
	c, err := getCalendar(r.Context(), h.DB, org)
	if errors.Is(err, errNoCalendar) {
		http.Error(w, "Google Calendar isn't connected", http.StatusNotFound)
		return
	}
	if err != nil {
		logs.Errorf("google calendar: test: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.check(w, r, c.AdminEmail, c.HolidaysCalendar) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// check asks Google; a refusal is a 400 with what to fix, Google being unreachable a 502.
func (h CalendarSystem) check(w http.ResponseWriter, r *http.Request, adminEmail, holidays string) bool {
	err := h.Google.Check(r.Context(), adminEmail, holidays)
	var refused CalendarError
	switch {
	case err == nil:
		return true
	case errors.As(err, &refused):
		http.Error(w, refused.Message, http.StatusBadRequest)
	default:
		logs.Warnf("google calendar: check: %v", err)
		http.Error(w, "couldn't reach Google; try again in a minute", http.StatusBadGateway)
	}
	return false
}

// Disconnect: DELETE /api/integrations/google-calendar. Org admins only. The time away it found goes
// with it (TL-406: time_away rows cascade from the connection).
func (h CalendarSystem) Disconnect(w http.ResponseWriter, r *http.Request) {
	if !h.on(w, r) {
		return
	}
	org, _, ok := h.Admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	if _, err := h.DB.Exec(r.Context(), `DELETE FROM calendar_connections WHERE org_id = $1`, org); err != nil {
		logs.Errorf("google calendar: disconnect: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

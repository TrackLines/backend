package integrations

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

var (
	errFlagsNotConnected = httpx.Code("flags_not_connected", "connect flags.gg first (Settings → Integrations)")
	errFlagsUnavailable  = httpx.Code("flags_unavailable", "flags.gg didn't answer: try again in a moment")
	errBadFlagsToken     = httpx.Code("bad_flags_token", "flags.gg didn't accept that token: make one in flags.gg under Company → Integrations")
	errBadFlagsAgent     = httpx.Code("bad_flags_agent", "that agent isn't one this flags.gg token can see")
	errBadCleanupDays    = httpx.Code("bad_flag_cleanup_days", "cleanup_days must be 1–365")
	errBadFlagChange     = httpx.Code("bad_flag_toggle", "environment_id and one of enabled, rollout (0–100) or rules are required; a rule needs an attribute, an operator (in, not_in, contains, starts_with, ends_with) and 1–1000 values (at most 50 rules)")
	errFlagNotCreated    = httpx.Code("flag_not_created", "the flag isn't in flags.gg yet: it's made when the ticket goes onto a board")
)

// FlagsSystem: connecting flags.gg, each project's agent, and a ticket's flag panel (TL-786, TL-787).
type FlagsSystem struct {
	h       System
	api     FlagsAPI
	tickets tickets.Store
}

// Flags is the flags.gg handlers, sharing the integrations' store and admin checks.
func (h System) Flags(api FlagsAPI) FlagsSystem {
	return FlagsSystem{h: h, api: api, tickets: tickets.Store{DB: h.store.DB}}
}

// flagsErr answers err from a flags.gg call.
func flagsErr(w http.ResponseWriter, r *http.Request, err error) {
	var fe FlagsError
	switch {
	case errors.Is(err, ErrNotConnected):
		httpx.Error(w, r, http.StatusConflict, errFlagsNotConnected)
	case errors.Is(err, errUnavailable):
		httpx.Error(w, r, http.StatusBadGateway, errFlagsUnavailable)
	case errors.As(err, &fe) && fe.Status == http.StatusNotFound:
		httpx.Error(w, r, http.StatusNotFound, errFlagNotCreated)
	case errors.As(err, &fe) && (fe.Status == http.StatusUnauthorized || fe.Status == http.StatusForbidden):
		httpx.Error(w, r, http.StatusBadGateway, errBadFlagsToken)
	case errors.As(err, &fe):
		httpx.Error(w, r, http.StatusConflict, httpx.Code("flags_refused", fe.Msg))
	case errors.Is(err, tickets.ErrNotFound):
		http.NotFound(w, r)
	default:
		logs.Errorf("flags: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Connection: GET /api/integrations/flags: connected or not and, when it is, the agents the token sees
// (agents_error says why they couldn't be listed). Never the token.
func (f FlagsSystem) Connection(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrSignInOrg)
		return
	}
	c, err := f.h.store.Get(r.Context(), org, Flags)
	if errors.Is(err, ErrNotConnected) {
		httpx.JSON(w, http.StatusOK, map[string]any{"provider": Flags, "connected": false})
		return
	}
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	out := map[string]any{"provider": Flags, "connected": true, "base_url": c.BaseURL, "updated_by": c.UpdatedBy, "updated_at": c.UpdatedAt}
	if base, token, err := f.h.store.credentials(r.Context(), org, Flags); err == nil {
		if agents, err := f.api.Agents(r.Context(), base, token); err == nil {
			out["agents"] = agents
		} else {
			out["agents_error"] = err.Error()
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Connect: PUT /api/integrations/flags {"token": "flg_…", "base_url"?}, org admins. The token is
// checked with flags.gg before it's kept.
func (f FlagsSystem) Connect(w http.ResponseWriter, r *http.Request) {
	org, user, ok := f.h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Token, in.BaseURL = strings.TrimSpace(in.Token), strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if in.BaseURL == "" {
		in.BaseURL = DefaultFlagsURL
	}
	if in.Token == "" || len(in.Token) > 500 || !validBaseURL(in.BaseURL) {
		httpx.Error(w, r, http.StatusBadRequest, errBadFlagsToken)
		return
	}
	agents, err := f.api.Agents(r.Context(), in.BaseURL, in.Token)
	var fe FlagsError
	if errors.As(err, &fe) {
		httpx.Error(w, r, http.StatusBadRequest, errBadFlagsToken)
		return
	}
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	if err := f.h.store.Connect(r.Context(), org, Flags, in.BaseURL, in.Token, user); err != nil {
		if errors.Is(err, secrets.ErrNoKey) {
			httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("integrations_not_configured", "integrations are not configured on this server"))
			return
		}
		flagsErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"provider": Flags, "connected": true, "base_url": in.BaseURL, "agents": agents})
}

// ProjectSettings: GET /api/projects/{id}/flags: the project's default agent and how many days after
// production its "Remove flag" tickets are due.
func (f FlagsSystem) ProjectSettings(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrSignInOrg)
		return
	}
	var agent *string
	var days int
	err := f.h.store.DB.QueryRow(r.Context(), `SELECT flags_agent_id, flag_cleanup_days FROM projects WHERE id::text = $1 AND owner_clerk_id = $2`,
		r.PathValue("id"), org).Scan(&agent, &days)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"agent_id": agent, "cleanup_days": days})
}

// SetProjectSettings: PUT /api/projects/{id}/flags {"agent_id"?, "cleanup_days"?}, org admins; the
// agent must be one the org's token sees ("" clears it).
func (f FlagsSystem) SetProjectSettings(w http.ResponseWriter, r *http.Request) {
	org, _, ok := f.h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		AgentID     *string `json:"agent_id"`
		CleanupDays *int    `json:"cleanup_days"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.CleanupDays != nil && (*in.CleanupDays < 1 || *in.CleanupDays > 365) {
		httpx.Error(w, r, http.StatusBadRequest, errBadCleanupDays)
		return
	}
	if in.AgentID != nil && !f.knownAgent(w, r, org, *in.AgentID) {
		return
	}
	tag, err := f.h.store.DB.Exec(r.Context(), `UPDATE projects SET
			flags_agent_id = CASE WHEN $3::text IS NULL THEN flags_agent_id ELSE NULLIF($3, '') END,
			flag_cleanup_days = COALESCE($4, flag_cleanup_days)
		WHERE id::text = $1 AND owner_clerk_id = $2`, r.PathValue("id"), org, in.AgentID, in.CleanupDays)
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	f.ProjectSettings(w, r)
}

// knownAgent is true when agent is "" or one org's flags.gg token sees; otherwise the answer is written.
func (f FlagsSystem) knownAgent(w http.ResponseWriter, r *http.Request, org, agent string) bool {
	if agent == "" {
		return true
	}
	base, token, err := f.h.store.credentials(r.Context(), org, Flags)
	if err != nil {
		flagsErr(w, r, err)
		return false
	}
	agents, err := f.api.Agents(r.Context(), base, token)
	if err != nil {
		flagsErr(w, r, err)
		return false
	}
	for _, a := range agents {
		if a.AgentID == agent {
			return true
		}
	}
	httpx.Error(w, r, http.StatusBadRequest, errBadFlagsAgent)
	return false
}

// SetBoardAgent: PUT /api/boards/{id}/flags {"agent_id"}, the board's managers (TL-804). flags.gg agents
// are per board in practice (frontend agent = Frontend board); "" falls back to the project's default.
func (f FlagsSystem) SetBoardAgent(w http.ResponseWriter, r *http.Request) {
	if !f.h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
	org := auth.OrgID(r.Context())
	var in struct {
		AgentID *string `json:"agent_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.AgentID == nil {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("flags_agent_required", "agent_id is required (\"\" for the project's)"))
		return
	}
	if !f.knownAgent(w, r, org, *in.AgentID) {
		return
	}
	var agent *string
	err := f.h.store.DB.QueryRow(r.Context(), `UPDATE boards SET flags_agent_id = NULLIF($3, '') WHERE id::text = $1 AND owner_clerk_id = $2
		RETURNING flags_agent_id`, r.PathValue("id"), org, *in.AgentID).Scan(&agent)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"agent_id": agent})
}

// ticketCreds: the org, its flags.gg credentials and ticket {id}, or an answer already written.
func (f FlagsSystem) ticketCreds(w http.ResponseWriter, r *http.Request) (base, token string, t *tickets.FlagTicket, ok bool) {
	org := auth.OrgID(r.Context())
	if org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrSignInOrg)
		return "", "", nil, false
	}
	t, err := f.tickets.FlagTicketOf(r.Context(), org, r.PathValue("id"))
	if err == nil {
		base, token, err = f.h.store.credentials(r.Context(), org, Flags)
	}
	if err != nil {
		flagsErr(w, r, err)
		return "", "", nil, false
	}
	return base, token, t, true
}

// TicketFlag: GET /api/tickets/{id}/flag: the ticket's flag in each environment, read from flags.gg
// each time (it doesn't tell us about changes made there yet).
func (f FlagsSystem) TicketFlag(w http.ResponseWriter, r *http.Request) {
	base, token, t, ok := f.ticketCreds(w, r)
	if !ok {
		return
	}
	tf, err := f.api.Get(r.Context(), base, token, t.Key)
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, tf)
}

// SetTicketFlag: PATCH /api/tickets/{id}/flag {"environment_id", "enabled"?, "rollout"?, "rules"?}: in one
// environment it has reached, turns it on or off and/or specialises it (a rollout percentage, targeting rules).
func (f FlagsSystem) SetTicketFlag(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EnvironmentID string `json:"environment_id"`
		FlagChange
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.EnvironmentID == "" || !in.valid() {
		httpx.Error(w, r, http.StatusBadRequest, errBadFlagChange)
		return
	}
	base, token, t, ok := f.ticketCreds(w, r)
	if !ok {
		return
	}
	tf, err := f.api.Set(r.Context(), base, token, t.Key, in.EnvironmentID, in.FlagChange)
	if err != nil {
		flagsErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, tf)
}

// RemoveTicketFlag: DELETE /api/tickets/{id}/flag: "Remove flag": the code no longer checks it, so it's
// archived in flags.gg (queued) and the ticket stops needing one.
func (f FlagsSystem) RemoveTicketFlag(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrSignInOrg)
		return
	}
	if err := f.tickets.RemoveFlag(r.Context(), org, r.PathValue("id")); err != nil {
		flagsErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package live

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Hosted refinement on project {id} (TL-223) and hosted standup on board {id} (TL-225): routes are
// declared in internal/service.go.

const (
	refinement = "refinement"
	standup    = "standup"
)

// project checks the caller can see project {id} (it's in their org) and returns its scope and the
// caller; false means an error was written.
func (s System) project(w http.ResponseWriter, r *http.Request) (scope, string, bool) {
	if s.VK == nil {
		http.Error(w, "live sessions unavailable", http.StatusServiceUnavailable)
		return scope{}, "", false
	}
	user, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return scope{}, "", false
	}
	var exists bool
	if err := s.Boards.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM projects WHERE id::text = $1 AND owner_clerk_id = $2)`,
		r.PathValue("id"), auth.OrgID(r.Context())).Scan(&exists); err != nil || !exists {
		http.NotFound(w, r)
		return scope{}, "", false
	}
	return projectScope(refinement, r.PathValue("id")), user, true
}

func hostedErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoSession):
		http.Error(w, "no session running", http.StatusNotFound)
	case errors.Is(err, ErrSessionRunning):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrNotHost):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrNotJoined):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		logs.Errorf("live: hosted session: %v", err)
		http.Error(w, "live session unavailable", http.StatusServiceUnavailable)
	}
}

// ProjectStream: GET /api/projects/{id}/live, the project's live events (the refinement session).
func (s System) ProjectStream(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.project(w, r)
	if !ok {
		return
	}
	s.stream(w, r, sc.events, sc.channel)
}

// RefinementStart: POST /api/projects/{id}/live/refinement makes the caller the host (409 if one's running).
func (s System) RefinementStart(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.project(w, r)
	if !ok {
		return
	}
	h, err := s.startHosted(r.Context(), sc, refinement, user)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, h)
}

// RefinementGet: GET /api/projects/{id}/live/refinement, the running session (404 when none).
func (s System) RefinementGet(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.project(w, r)
	if !ok {
		return
	}
	h, err := s.hosted(r.Context(), sc, refinement)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

// RefinementJoin: POST /api/projects/{id}/live/refinement/join joins, and keeps the caller there (every 30s).
func (s System) RefinementJoin(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.project(w, r)
	if !ok {
		return
	}
	h, err := s.joinHosted(r.Context(), sc, refinement, user)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

// RefinementLeave: POST /api/projects/{id}/live/refinement/leave.
func (s System) RefinementLeave(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.project(w, r)
	if !ok {
		return
	}
	if err := s.leaveHosted(r.Context(), sc, refinement, user); err != nil {
		hostedErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RefinementFocus: PUT /api/projects/{id}/live/refinement/focus {ticketId, filters}, host only.
func (s System) RefinementFocus(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.project(w, r)
	if !ok {
		return
	}
	var f Focus
	if !httpx.Decode(w, r, &f) {
		return
	}
	h, err := s.focusHosted(r.Context(), sc, refinement, user, f)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

// RefinementHost: PUT /api/projects/{id}/live/refinement/host {"user_id"}: hand over, or take over
// from a host who has gone.
func (s System) RefinementHost(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.project(w, r)
	if !ok {
		return
	}
	var in struct {
		UserID string `json:"user_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	h, err := s.handOver(r.Context(), sc, refinement, user, in.UserID)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

// RefinementEnd: DELETE /api/projects/{id}/live/refinement, host only.
func (s System) RefinementEnd(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.project(w, r)
	if !ok {
		return
	}
	h, err := s.hosted(r.Context(), sc, refinement)
	if err != nil {
		hostedErr(w, err)
		return
	}
	if h.Host != user {
		hostedErr(w, ErrNotHost)
		return
	}
	if err := s.endHosted(r.Context(), sc, refinement); err != nil {
		hostedErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// board checks the caller can see board {id} and returns its standup scope and the caller.
func (s System) board(w http.ResponseWriter, r *http.Request) (scope, string, bool) {
	if !s.authorize(w, r) {
		return scope{}, "", false
	}
	user, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return scope{}, "", false
	}
	return boardScope(standup, r.PathValue("id")), user, true
}

// StandupSession: GET /api/boards/{id}/live/standup/session, who's here and what the host has open.
func (s System) StandupSession(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.board(w, r)
	if !ok {
		return
	}
	h, err := s.hosted(r.Context(), sc, standup)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

// StandupJoin: POST /api/boards/{id}/live/standup/join joins the standup, and keeps the caller there (every 30s).
func (s System) StandupJoin(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.board(w, r)
	if !ok {
		return
	}
	h, err := s.joinHosted(r.Context(), sc, standup, user)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

// StandupLeave: POST /api/boards/{id}/live/standup/leave.
func (s System) StandupLeave(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.board(w, r)
	if !ok {
		return
	}
	if err := s.leaveHosted(r.Context(), sc, standup, user); err != nil {
		hostedErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StandupFocus: PUT /api/boards/{id}/live/standup/focus {ticketId, filters}, host only.
func (s System) StandupFocus(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.board(w, r)
	if !ok {
		return
	}
	var f Focus
	if !httpx.Decode(w, r, &f) {
		return
	}
	h, err := s.focusHosted(r.Context(), sc, standup, user, f)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

// StandupHost: PUT /api/boards/{id}/live/standup/host {"user_id"}: hand over, or take over from a host who has gone.
func (s System) StandupHost(w http.ResponseWriter, r *http.Request) {
	sc, user, ok := s.board(w, r)
	if !ok {
		return
	}
	var in struct {
		UserID string `json:"user_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	h, err := s.handOver(r.Context(), sc, standup, user, in.UserID)
	if err != nil {
		hostedErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h)
}

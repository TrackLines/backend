package teams

import (
	"net/http"

	"github.com/tracklines/backend/internal/httpx"
)

// AddLeader: PUT /api/teams/{id}/leaders/{userID}. Org admins only; the user must already be on the team.
func (h System) AddLeader(w http.ResponseWriter, r *http.Request) { h.leader(w, r, true) }

// RemoveLeader: DELETE /api/teams/{id}/leaders/{userID}. Org admins only; they stay on the team.
func (h System) RemoveLeader(w http.ResponseWriter, r *http.Request) { h.leader(w, r, false) }

func (h System) leader(w http.ResponseWriter, r *http.Request, lead bool) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var exists bool
	if err := h.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM teams WHERE id = $1 AND org_id = $2)`, r.PathValue("id"), org).Scan(&exists); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	tag, err := h.db.Exec(r.Context(), `UPDATE team_members SET leader = $3 WHERE team_id = $1 AND user_clerk_id = $2`, r.PathValue("id"), r.PathValue("userID"), lead)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "add them to the team before making them a leader", http.StatusUnprocessableEntity)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetBoardTeam: PUT /api/boards/{id}/team {"team_id": "…" | null}. Org admins only. The team must be
// in the org and already linked to the board's project; null unlinks.
func (h System) SetBoardTeam(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		TeamID *string `json:"team_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var board, team bool
	err := h.db.QueryRow(r.Context(), `SELECT
			EXISTS (SELECT 1 FROM boards WHERE id::text = $1 AND owner_clerk_id = $2),
			$3::text IS NULL OR EXISTS (SELECT 1 FROM boards b JOIN project_teams pt ON pt.project_id = b.project_id
				WHERE b.id::text = $1 AND pt.team_id::text = $3 AND pt.org_id = $2)`,
		r.PathValue("id"), org, in.TeamID).Scan(&board, &team)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !board {
		http.NotFound(w, r)
		return
	}
	if !team {
		http.Error(w, "add the team to this board's project first", http.StatusUnprocessableEntity)
		return
	}
	if _, err := h.db.Exec(r.Context(), `UPDATE boards SET team_id = $2::uuid, updated_at = now() WHERE id::text = $1`, r.PathValue("id"), in.TeamID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

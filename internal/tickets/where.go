package tickets

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Where: GET /api/tickets/{id}/org says which organization ticket {id} is in, so a link opened while
// another of your orgs is active can offer to switch (TL-276) instead of "not found". It only answers
// a signed-in person who belongs to that organization; to anyone else the ticket doesn't exist.
func (h System) Where(w http.ResponseWriter, r *http.Request) {
	user, signedIn := auth.UserID(r.Context())
	if !signedIn || auth.ViaAPIKey(r.Context()) {
		http.NotFound(w, r)
		return
	}
	var org string
	err := h.store.DB.QueryRow(r.Context(), `SELECT p.owner_clerk_id FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id::text = $1`, r.PathValue("id")).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	member, err := h.isOrganizationMember(r.Context(), org, user)
	if err != nil {
		logs.Errorf("tickets: where: check membership: %v", err)
		httpx.Error(w, r, http.StatusBadGateway, httpx.ErrVerifyMembership)
		return
	}
	if !member {
		http.NotFound(w, r)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"org_id": org})
}

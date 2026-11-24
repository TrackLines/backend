package users

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// LatestTickets is the "Latest tickets" tab on the home Mentions card: Limit tickets (0 = off,
// else 1–20) newest first across every project, at most PerProject from any one.
type LatestTickets struct {
	Limit      int `json:"limit"`
	PerProject int `json:"per_project"`
}

// GetLatestTickets: GET /api/me/latest-tickets → {"limit": 0, "per_project": 3}.
func (s Store) GetLatestTickets(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.UserID(r.Context())
	out := LatestTickets{PerProject: 3} // no row yet: the defaults
	if err := s.DB.QueryRow(r.Context(), `SELECT latest_tickets, latest_per_project FROM users WHERE clerk_id = $1`, id).
		Scan(&out.Limit, &out.PerProject); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// PutLatestTickets: PUT /api/me/latest-tickets {"limit": 0–20, "per_project": 1–20}.
func (s Store) PutLatestTickets(w http.ResponseWriter, r *http.Request) {
	var in LatestTickets
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Limit < 0 || in.Limit > 20 || in.PerProject < 1 || in.PerProject > 20 {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_latest_tickets", "limit must be 0–20 and per_project 1–20"))
		return
	}
	id, _ := auth.UserID(r.Context())
	if _, err := s.DB.Exec(r.Context(), `UPDATE users SET latest_tickets = $2, latest_per_project = $3, updated_at = now() WHERE clerk_id = $1`,
		id, in.Limit, in.PerProject); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

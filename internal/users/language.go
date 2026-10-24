package users

import (
	"context"
	"net/http"
	"slices"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Languages the frontend is translated into (TL-549); English is the source.
var Languages = []string{"en", "nl"}

// Language is the user's chosen language, or nil to follow the browser.
func (s Store) Language(ctx context.Context, clerkID string) (*string, error) {
	var lang *string
	err := s.DB.QueryRow(ctx, `SELECT language FROM users WHERE clerk_id = $1`, clerkID).Scan(&lang)
	return lang, err
}

// SetLanguage sets it (nil: follow the browser again).
func (s Store) SetLanguage(ctx context.Context, clerkID string, lang *string) error {
	_, err := s.DB.Exec(ctx, `UPDATE users SET language = $2, updated_at = now() WHERE clerk_id = $1`, clerkID, lang)
	return err
}

// GetLanguage: GET /api/me/language → {"language": "nl" | null}.
func (s Store) GetLanguage(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.UserID(r.Context())
	lang, err := s.Language(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]*string{"language": lang})
}

// PutLanguage: PUT /api/me/language {"language": "nl" | null}; only languages we translate into.
func (s Store) PutLanguage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Language *string `json:"language"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Language != nil && !slices.Contains(Languages, *in.Language) {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_language", "language must be one of en, nl, or null"))
		return
	}
	id, _ := auth.UserID(r.Context())
	if err := s.SetLanguage(r.Context(), id, in.Language); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

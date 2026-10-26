// Package staff is the admin panel's access (TL-573): ChewedFeed's own people, the members of the Clerk
// org in STAFF_ORG_ID, whatever org they have active. Everyone else, API keys included, gets 404 so the
// panel isn't advertised.
package staff

import (
	"context"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// Member reports whether user belongs to org in Clerk.
type Member func(ctx context.Context, org, user string) (bool, error)

// Guard decides who's staff. Org "" (not configured) means nobody is; On is the admin-panel flag.
type Guard struct {
	Org    string
	Member Member
	On     func() bool
}

// Only lets staff through to next; everyone else gets 404.
func (g Guard) Only(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserID(r.Context())
		if g.Org == "" || !g.On() || !ok || auth.ViaAPIKey(r.Context()) {
			http.NotFound(w, r)
			return
		}
		in, err := g.Member(r.Context(), g.Org, user)
		if err != nil {
			if httpx.Canceled(err) || r.Context().Err() != nil {
				return // the client left
			}
			logs.Errorf("staff: membership: %v", err)
			httpx.Error(w, r, http.StatusServiceUnavailable, httpx.ErrVerifyMembership)
			return
		}
		if !in {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Me tells the web app to show the panel link: only staff get past Only, so it's always true.
func Me(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]bool{"staff": true})
}

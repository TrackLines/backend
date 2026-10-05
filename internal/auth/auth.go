package auth

import (
	"context"
	"net/http"

	"github.com/clerk/clerk-sdk-go/v2"
	clerkhttp "github.com/clerk/clerk-sdk-go/v2/http"
)

// Init sets the Clerk secret key (cfg.Clerk.Key) used to fetch JWKS.
func Init(secretKey string) { clerk.SetKey(secretKey) }

// Optional verifies a Bearer session token when present; anon requests pass through.
var Optional = clerkhttp.WithHeaderAuthorization()

// Required is Optional plus 401 when there is no valid session.
func Required(next http.Handler) http.Handler {
	return Optional(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserID(r.Context()); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// UserID returns the Clerk user id (JWT sub) of the verified session.
func UserID(ctx context.Context) (string, bool) {
	c, ok := clerk.SessionClaimsFromContext(ctx)
	if !ok || c == nil || c.Subject == "" {
		return "", false
	}
	return c.Subject, true
}

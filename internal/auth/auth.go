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

// SessionRequired is Required but refuses API-key callers (403): for actions only a
// signed-in person may take, like creating or revoking API keys.
func SessionRequired(next http.Handler) http.Handler {
	return Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ViaAPIKey(r.Context()) {
			http.Error(w, "requires a signed-in session, not an API key", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

type apiKeyUserContextKey struct{}

type apiKeyUser struct {
	owner string
	name  string
}

// WithAPIKeyUser marks the request as made with an API key acting as userID.
func WithAPIKeyUser(ctx context.Context, userID, name string) context.Context {
	return context.WithValue(ctx, apiKeyUserContextKey{}, apiKeyUser{owner: userID, name: name})
}

// ViaAPIKey reports whether the caller authenticated with an API key.
func ViaAPIKey(ctx context.Context) bool {
	_, ok := ctx.Value(apiKeyUserContextKey{}).(apiKeyUser)
	return ok
}

// UserID returns the Clerk user id of the caller: the API key's owner, or the
// subject of the verified Clerk session.
func UserID(ctx context.Context) (string, bool) {
	if key, ok := ctx.Value(apiKeyUserContextKey{}).(apiKeyUser); ok && key.owner != "" {
		return key.owner, true
	}
	c, ok := clerk.SessionClaimsFromContext(ctx)
	if !ok || c == nil || c.Subject == "" {
		return "", false
	}
	return c.Subject, true
}

// ActorID returns the API key's configured agent name, or the Clerk user ID for
// interactive callers. Ticket attribution and assignment use this stable label.
func ActorID(ctx context.Context) string {
	if key, ok := ctx.Value(apiKeyUserContextKey{}).(apiKeyUser); ok && key.name != "" {
		return key.name
	}
	id, _ := UserID(ctx)
	return id
}

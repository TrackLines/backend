package auth

import (
	"context"
	"net/http"

	"github.com/clerk/clerk-sdk-go/v2"
	clerkhttp "github.com/clerk/clerk-sdk-go/v2/http"
)

// Init sets the Clerk secret key (cfg.Clerk.Key) used to fetch JWKS.
func Init(secretKey string) { clerk.SetKey(secretKey) }

// orgClaims reads the active organization from Clerk's v2 session tokens ({"o": {"id": …}});
// the SDK (v2.7) only knows the v1 org_id claim.
type orgClaims struct {
	O struct {
		ID string `json:"id"`
	} `json:"o"`
}

// Optional verifies a Bearer session token when present; anon requests pass through.
var Optional = clerkhttp.WithHeaderAuthorization(clerkhttp.CustomClaimsConstructor(func(context.Context) any { return &orgClaims{} }))

// Required is Optional plus 401 when there is no valid session, and 403 when no organization
// is active: projects, boards and roadmaps belong to an org.
func Required(next http.Handler) http.Handler {
	return Optional(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserID(r.Context()); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if OrgID(r.Context()) == "" {
			http.Error(w, "choose an organization first", http.StatusForbidden)
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
	org   string
	name  string
	kind  string
}

// WithAPIKey marks the request as made with an API key: it acts as userID inside org.
func WithAPIKey(ctx context.Context, userID, org, name, kind string) context.Context {
	if kind == "" {
		kind = "ai" // keys historically represented agents
	}
	return context.WithValue(ctx, apiKeyUserContextKey{}, apiKeyUser{owner: userID, org: org, name: name, kind: kind})
}

// WithAPIKeyUser is WithAPIKey with the user standing in as its own org.
// ponytail: kept so the store tests (owner "t1", "lb1"…) don't all need an org; real keys use WithAPIKey.
func WithAPIKeyUser(ctx context.Context, userID, name string, kind ...string) context.Context {
	k := ""
	if len(kind) > 0 {
		k = kind[0]
	}
	return WithAPIKey(ctx, userID, userID, name, k)
}

// ViaAPIKey reports whether the caller authenticated with an API key.
func ViaAPIKey(ctx context.Context) bool {
	_, ok := ctx.Value(apiKeyUserContextKey{}).(apiKeyUser)
	return ok
}

// APIKeyKind identifies whether this API-key request is an AI agent or service integration.
func APIKeyKind(ctx context.Context) string {
	key, _ := ctx.Value(apiKeyUserContextKey{}).(apiKeyUser)
	return key.kind
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

// OrgID returns the Clerk organization that owns the caller's data: the API key's org, or the
// session's active organization. "" when none is active.
func OrgID(ctx context.Context) string {
	if key, ok := ctx.Value(apiKeyUserContextKey{}).(apiKeyUser); ok {
		return key.org
	}
	c, ok := clerk.SessionClaimsFromContext(ctx)
	if !ok || c == nil {
		return ""
	}
	if c.ActiveOrganizationID != "" {
		return c.ActiveOrganizationID
	}
	if o, ok := c.Custom.(*orgClaims); ok {
		return o.O.ID
	}
	return ""
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

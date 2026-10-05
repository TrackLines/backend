package users

import (
	"context"
	"net/http"
	"sync"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// Ensure makes sure a signed-in user has a users row (other tables FK on clerk_id).
// Apply after auth.Optional/Required. Anon requests pass straight through.
func Ensure(db *pgxpool.Pool) func(http.Handler) http.Handler {
	s := Store{DB: db}
	var known sync.Map // ponytail: per-instance cache, never evicts; fine until user count is huge
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := auth.UserID(r.Context())
			if ok {
				if _, seen := known.Load(id); !seen {
					if err := ensure(r.Context(), s, id); err != nil {
						logs.Errorf("users: ensure %s: %v", id, err)
						http.Error(w, "internal error", http.StatusInternalServerError)
						return
					}
					known.Store(id, struct{}{})
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func lookupEmail(ctx context.Context, id string) (string, error) {
	u, err := user.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return primaryEmail(u), nil
}

func ensure(ctx context.Context, s Store, id string) error {
	if ok, err := s.Exists(ctx, id); err != nil || ok {
		return err
	}
	email, err := lookupEmail(ctx, id)
	if err != nil {
		return err
	}
	return s.Upsert(ctx, id, email)
}

func primaryEmail(u *clerk.User) string {
	for _, e := range u.EmailAddresses {
		if u.PrimaryEmailAddressID != nil && e.ID == *u.PrimaryEmailAddressID {
			return e.EmailAddress
		}
	}
	if len(u.EmailAddresses) > 0 {
		return u.EmailAddresses[0].EmailAddress
	}
	return ""
}

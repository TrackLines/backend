package apikeys

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

type System struct{ store Store }

// NewSystem exposes the API key handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

// Middleware lets `Authorization: Bearer tl_…` act as the key's owner. Anything else
// (Clerk JWTs, anon) passes through untouched. A bad or revoked tl_ key is a 401, never anon.
func Middleware(db *pgxpool.Pool) func(http.Handler) http.Handler {
	s := Store{DB: db}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || !strings.HasPrefix(key, Prefix) {
				next.ServeHTTP(w, r)
				return
			}
			owner, org, name, kind, err := s.Owner(r.Context(), key)
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					logs.Errorf("apikeys: lookup: %v", err)
				}
				http.Error(w, "invalid api key", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAPIKey(r.Context(), owner, org, name, kind)))
		})
	}
}

func (h System) List(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.List(r.Context(), user, auth.OrgID(r.Context()))
	if err != nil {
		logs.Errorf("apikeys: list: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Create returns {"key": "tl_…", …}; the plaintext key is only ever shown in this response.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		http.Error(w, "name is required (e.g. the agent using it)", http.StatusBadRequest)
		return
	}
	if in.Kind != KindAI && in.Kind != KindService {
		http.Error(w, "kind must be ai or service", http.StatusBadRequest)
		return
	}
	user, _ := auth.UserID(r.Context())
	plain, k, err := h.store.Create(r.Context(), user, auth.OrgID(r.Context()), in.Name, in.Kind)
	if err != nil {
		logs.Errorf("apikeys: create: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusCreated, struct {
		Secret string `json:"key"`
		*Key
	}{plain, k})
}

func (h System) Revoke(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	if err := h.store.Revoke(r.Context(), r.PathValue("id"), user); err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logs.Errorf("apikeys: revoke: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

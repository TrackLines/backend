package apikeys

import (
	"context"
	"errors"
	"fmt"
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

// Member reports whether user still belongs to org in Clerk (cached briefly by the caller).
type Member func(ctx context.Context, org, user string) (bool, error)

// Middleware lets `Authorization: Bearer tl_…` act as the key's owner. Anything else
// (Clerk JWTs, anon) passes through untouched. A bad or revoked tl_ key is a 401, never anon.
// With member set, a key whose owner has left its org is revoked on use and refused, so a missed
// Clerk webhook can't leave a leaver with access (TL-160); if Clerk can't say, it's a 503.
func Middleware(db *pgxpool.Pool, member Member) func(http.Handler) http.Handler {
	s := Store{DB: db}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || !strings.HasPrefix(key, Prefix) {
				next.ServeHTTP(w, r)
				return
			}
			owner, org, name, kind, err := s.Owner(r.Context(), key)
			if errors.Is(err, ErrExpired) {
				http.Error(w, "api key expired: create a new one in Settings → API keys", http.StatusUnauthorized)
				return
			}
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					logs.Errorf("apikeys: lookup: %v", err)
				}
				http.Error(w, "invalid api key", http.StatusUnauthorized)
				return
			}
			if member != nil && org != "" {
				still, err := member(r.Context(), org, owner)
				if err != nil {
					logs.Errorf("apikeys: check organization membership: %v", err)
					http.Error(w, "could not verify organization membership", http.StatusServiceUnavailable)
					return
				}
				if !still {
					if err := s.RevokeKey(r.Context(), key); err != nil {
						logs.Errorf("apikeys: revoke a leaver's key: %v", err)
					}
					http.Error(w, "invalid api key", http.StatusUnauthorized)
					return
				}
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
// Agent keys (kind ai) say their client, and a label for "other"; their name is generated (TL-269).
// Service keys (scripts, integrations) are named by hand.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Client string `json:"client"`
		Label  string `json:"label"`
		Days   *int   `json:"expires_in_days"` // left out: DefaultDays
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	days := DefaultDays
	if in.Days != nil {
		days = *in.Days
	}
	if days < 1 || days > MaxDays {
		http.Error(w, fmt.Sprintf("expires_in_days must be from 1 to %d", MaxDays), http.StatusBadRequest)
		return
	}
	if in.Kind != KindAI && in.Kind != KindService {
		http.Error(w, "kind must be ai or service", http.StatusBadRequest)
		return
	}
	user, _ := auth.UserID(r.Context())
	if in.Kind == KindAI {
		h.createAgent(w, r, user, in.Client, in.Label, days)
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		http.Error(w, "name is required (e.g. the script using it)", http.StatusBadRequest)
		return
	}
	// a key's name is who its work is recorded as (comments, tickets, claims), so it can't pose as a
	// person (user_…), as the system or an integration, or as another member's key (TL-247)
	if reservedName(in.Name) {
		http.Error(w, "that name is reserved: choose a name for the agent or script using the key", http.StatusBadRequest)
		return
	}
	taken, err := h.store.NameTakenByOther(r.Context(), auth.OrgID(r.Context()), user, in.Name)
	if err != nil {
		logs.Errorf("apikeys: create: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if taken {
		http.Error(w, "someone else in this organization already has a key with that name: choose another", http.StatusConflict)
		return
	}
	plain, k, err := h.store.Create(r.Context(), user, auth.OrgID(r.Context()), in.Name, in.Kind, nil, nil, days)
	created(w, plain, k, err)
}

// createAgent mints an agent key for client, under a generated name nobody in the org has used.
func (h System) createAgent(w http.ResponseWriter, r *http.Request, user, client, label string, days int) {
	if !ValidClient(client) {
		http.Error(w, "client must be one of claude-code, claude-desktop, codex, cursor, vscode, gemini or other", http.StatusBadRequest)
		return
	}
	var lbl *string
	if label = strings.TrimSpace(label); client == "other" && label != "" {
		if len([]rune(label)) > MaxLabel {
			http.Error(w, fmt.Sprintf("the agent's name can be up to %d characters", MaxLabel), http.StatusBadRequest)
			return
		}
		if reservedName(label) {
			http.Error(w, "that name is reserved: choose the agent's name", http.StatusBadRequest)
			return
		}
		lbl = &label
	}
	org := auth.OrgID(r.Context())
	for range 5 {
		name, err := generatedName(client)
		if err != nil {
			created(w, "", nil, err)
			return
		}
		if used, err := h.store.NameInUse(r.Context(), org, name); err != nil || used {
			if err != nil {
				created(w, "", nil, err)
				return
			}
			continue
		}
		plain, k, err := h.store.Create(r.Context(), user, org, name, KindAI, &client, lbl, days)
		created(w, plain, k, err)
		return
	}
	created(w, "", nil, errors.New("no free agent name after 5 tries"))
}

func created(w http.ResponseWriter, plain string, k *Key, err error) {
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

// reservedName: names a key mustn't take, because work is recorded under them already: people
// (Clerk ids, user_…), imported work (legacy), and the PR integrations' comments (github, bitbucket).
func reservedName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(n, "user_") || n == "legacy" || n == "github" || n == "bitbucket"
}

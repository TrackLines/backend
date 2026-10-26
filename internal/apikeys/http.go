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

type System struct {
	store Store
	// IsAdmin: whether user is an admin of org; only admins make and revoke the org's service keys (TL-462). nil: nobody is
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
}

// admin reports whether the caller administers their organization (false when it can't tell).
func (h System) admin(r *http.Request) bool {
	user, _ := auth.UserID(r.Context())
	if h.IsAdmin == nil {
		return false
	}
	ok, err := h.IsAdmin(r.Context(), auth.OrgID(r.Context()), user)
	if err != nil {
		logs.Errorf("apikeys: admin check: %v", err)
	}
	return ok && err == nil
}

// NewSystem exposes the API key handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

// Member reports whether user still belongs to org in Clerk (cached briefly by the caller).
type Member func(ctx context.Context, org, user string) (bool, error)

// Middleware lets `Authorization: Bearer tl_…` act as the key's owner. A read-only key may only
// read (GET, HEAD, OPTIONS): anything else is a 403 (TL-450). Anything else
// (Clerk JWTs, anon) passes through untouched. A bad or revoked tl_ key is a 401, never anon.
// With member set, a key whose owner has left its org is revoked on use and refused, so a missed
// Clerk webhook can't leave a leaver with access (TL-160); if Clerk can't say, it's a 503.
func Middleware(db *pgxpool.Pool, member Member) func(http.Handler) http.Handler {
	s := Store{DB: db}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.Bearer(r)
			if !strings.HasPrefix(key, Prefix) {
				next.ServeHTTP(w, r)
				return
			}
			owner, org, name, kind, readOnly, err := s.Owner(r.Context(), key)
			if errors.Is(err, ErrExpired) {
				httpx.Error(w, r, http.StatusUnauthorized, httpx.Code("api_key_expired", "api key expired: create a new one in Settings → API keys"))
				return
			}
			if gone(r, err) {
				return
			}
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					logs.Errorf("apikeys: lookup: %v", err)
				}
				httpx.Error(w, r, http.StatusUnauthorized, httpx.Code("invalid_api_key", "invalid api key"))
				return
			}
			if member != nil && org != "" {
				still, err := member(r.Context(), org, owner)
				if gone(r, err) {
					return
				}
				if err != nil {
					logs.Errorf("apikeys: check organization membership: %v", err)
					httpx.Error(w, r, http.StatusServiceUnavailable, httpx.ErrVerifyMembership)
					return
				}
				if !still {
					if err := s.RevokeKey(r.Context(), key); err != nil {
						logs.Errorf("apikeys: revoke a leaver's key: %v", err)
					}
					httpx.Error(w, r, http.StatusUnauthorized, httpx.Code("invalid_api_key", "invalid api key"))
					return
				}
			}
			if readOnly && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
				httpx.Error(w, r, http.StatusForbidden, httpx.Code("read_only_key", "this api key is read-only"))
				return
			}
			if kind == KindPersonal {
				name = "" // a personal key is its owner: work is recorded as them, not the key
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAPIKey(r.Context(), owner, org, name, kind)))
		})
	}
}

// gone: the client left mid-request (TL-648), so a check that failed with it isn't a fault: nothing to
// log, nobody to answer, and the request still never gets through.
func gone(r *http.Request, err error) bool {
	return err != nil && (httpx.Canceled(err) || r.Context().Err() != nil)
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
// Service keys (scripts, integrations) are named by hand; so are personal keys (TL-450), whose name is
// only a label since they act as their owner. Any key can be read_only.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Client   string `json:"client"`
		Label    string `json:"label"`
		Days     *int   `json:"expires_in_days"` // left out: DefaultDays
		ReadOnly bool   `json:"read_only"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	days := DefaultDays
	if in.Days != nil {
		days = *in.Days
	}
	if days < 1 || days > MaxDays {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_key_expiry", fmt.Sprintf("expires_in_days must be from 1 to %d", MaxDays)))
		return
	}
	if in.Kind != KindAI && in.Kind != KindService && in.Kind != KindPersonal {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_key_kind", "kind must be ai, service or personal"))
		return
	}
	user, _ := auth.UserID(r.Context())
	if in.Kind == KindAI {
		h.createAgent(w, r, user, in.Client, in.Label, days, in.ReadOnly)
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("key_name_required", "name is required (e.g. the script using it)"))
		return
	}
	if in.Kind == KindPersonal {
		plain, k, err := h.store.Create(r.Context(), user, auth.OrgID(r.Context()), in.Name, in.Kind, nil, nil, days, in.ReadOnly)
		created(w, plain, k, err)
		return
	}
	// service keys belong to the organization: everyone sees them (and when they expire), admins make them (TL-462)
	if !h.admin(r) {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_make_service_keys", "only organization admins can make service keys"))
		return
	}
	// a key's name is who its work is recorded as (comments, tickets, claims), so it can't pose as a
	// person (user_…), as the system or an integration, or as another member's key (TL-247)
	if reservedName(in.Name) {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("reserved_key_name", "that name is reserved: choose a name for the agent or script using the key"))
		return
	}
	taken, err := h.store.NameTakenByOther(r.Context(), auth.OrgID(r.Context()), user, in.Name)
	if err != nil {
		logs.Errorf("apikeys: create: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if taken {
		httpx.Error(w, r, http.StatusConflict, httpx.Code("key_name_taken", "someone else in this organization already has a key with that name: choose another"))
		return
	}
	plain, k, err := h.store.Create(r.Context(), user, auth.OrgID(r.Context()), in.Name, in.Kind, nil, nil, days, in.ReadOnly)
	created(w, plain, k, err)
}

// createAgent mints an agent key for client, under a generated name nobody in the org has used.
func (h System) createAgent(w http.ResponseWriter, r *http.Request, user, client, label string, days int, readOnly bool) {
	if !ValidClient(client) {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_key_client", "client must be one of claude-code, claude-desktop, codex, cursor, vscode, gemini or other"))
		return
	}
	var lbl *string
	if label = strings.TrimSpace(label); client == "other" && label != "" {
		if len([]rune(label)) > MaxLabel {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("agent_name_too_long", fmt.Sprintf("the agent's name can be up to %d characters", MaxLabel)))
			return
		}
		if reservedName(label) {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("reserved_agent_name", "that name is reserved: choose the agent's name"))
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
		plain, k, err := h.store.Create(r.Context(), user, org, name, KindAI, &client, lbl, days, readOnly)
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
	err := h.store.Revoke(r.Context(), r.PathValue("id"), user)
	if errors.Is(err, ErrNotFound) && h.admin(r) { // not theirs: an admin can revoke any of the org's service keys (TL-462)
		err = h.store.RevokeService(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()))
	}
	if err != nil {
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

// Package clerkcache caches Clerk organization membership and user lists in Valkey, shared across backend
// instances. Use it only for reads that drive the UI (assignee pickers, member lists): checks that
// guard a change keep asking Clerk directly, so they're never decided on stale data.
package clerkcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/valkey-io/valkey-go"
)

type lister interface {
	List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)
}

// Memberships is a membership lister that answers from Valkey when it can. VK may be nil (Valkey
// not configured or unreachable): then every call goes to Clerk. Valkey errors never fail a call.
type Memberships struct {
	Next lister
	VK   valkey.Client
	TTL  time.Duration
}

func (m Memberships) List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	return cached(ctx, m.VK, m.TTL, "clerk:memberships:"+params.OrganizationID+":", params, m.Next.List)
}

type userLister interface {
	List(ctx context.Context, params *user.ListParams) (*clerk.UserList, error)
}

// Users is a user lister that answers from Valkey when it can, like Memberships.
type Users struct {
	Next userLister
	VK   valkey.Client
	TTL  time.Duration
}

func (u Users) List(ctx context.Context, params *user.ListParams) (*clerk.UserList, error) {
	return cached(ctx, u.VK, u.TTL, "clerk:users:", params, u.Next.List)
}

// cached answers fetch(params) from Valkey under prefix + a hash of params, filling it on a miss.
func cached[P any, T any](ctx context.Context, vk valkey.Client, ttl time.Duration, prefix string, params P, fetch func(context.Context, P) (*T, error)) (*T, error) {
	if vk == nil {
		return fetch(ctx, params)
	}
	raw, _ := json.Marshal(params)
	sum := sha256.Sum256(raw)
	key := prefix + hex.EncodeToString(sum[:8])

	if hit, err := vk.Do(ctx, vk.B().Get().Key(key).Build()).AsBytes(); err == nil {
		var out T
		if json.Unmarshal(hit, &out) == nil {
			return &out, nil
		}
	} else if !valkey.IsValkeyNil(err) {
		logs.Warnf("clerkcache: get: %v", err)
	}

	out, err := fetch(ctx, params)
	if err != nil || out == nil {
		return out, err
	}
	if b, err := json.Marshal(out); err == nil {
		if err := vk.Do(ctx, vk.B().Set().Key(key).Value(valkey.BinaryString(b)).Ex(ttl).Build()).Error(); err != nil {
			logs.Warnf("clerkcache: set: %v", err)
		}
	}
	return out, nil
}

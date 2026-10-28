// Package clerkcache caches Clerk organization membership and user lists in Valkey, shared across backend
// instances. Use it only for reads that drive the UI (assignee pickers, member lists): checks that
// guard a change keep asking Clerk directly, so they're never decided on stale data.
package clerkcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
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
	return cached(ctx, m.VK, m.TTL, "clerk:memberships:"+params.OrganizationID+":", params, retried(m.Next.List))
}

// Forget drops every cached member list of org, so a join or leave shows at once instead of after
// the TTL (TL-520). Valkey errors are logged, not returned: the TTL still clears it.
func (m Memberships) Forget(ctx context.Context, org string) {
	if m.VK == nil || org == "" {
		return
	}
	var cursor uint64
	for {
		res, err := m.VK.Do(ctx, m.VK.B().Scan().Cursor(cursor).Match("clerk:memberships:"+org+":*").Count(100).Build()).AsScanEntry()
		if err != nil {
			logs.Warnf("clerkcache: forget: %v", err)
			return
		}
		if len(res.Elements) > 0 {
			if err := m.VK.Do(ctx, m.VK.B().Del().Key(res.Elements...).Build()).Error(); err != nil {
				logs.Warnf("clerkcache: forget: %v", err)
			}
		}
		if cursor = res.Cursor; cursor == 0 {
			return
		}
	}
}

// retried asks again once when the connection to Clerk failed (reset, timeout) rather than Clerk
// answering with an error (TL-710): a list is a read, so a second try is safe.
func retried[P any, T any](fetch func(context.Context, P) (*T, error)) func(context.Context, P) (*T, error) {
	return func(ctx context.Context, params P) (*T, error) {
		out, err := fetch(ctx, params)
		var ne net.Error
		if err != nil && ctx.Err() == nil && errors.As(err, &ne) {
			return fetch(ctx, params)
		}
		return out, err
	}
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

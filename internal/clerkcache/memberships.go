// Package clerkcache caches Clerk organization membership lists in Valkey, shared across backend
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
	if m.VK == nil {
		return m.Next.List(ctx, params)
	}
	raw, _ := json.Marshal(params)
	sum := sha256.Sum256(raw)
	key := "clerk:memberships:" + params.OrganizationID + ":" + hex.EncodeToString(sum[:8])

	if cached, err := m.VK.Do(ctx, m.VK.B().Get().Key(key).Build()).AsBytes(); err == nil {
		var list clerk.OrganizationMembershipList
		if json.Unmarshal(cached, &list) == nil {
			return &list, nil
		}
	} else if !valkey.IsValkeyNil(err) {
		logs.Warnf("clerkcache: get: %v", err)
	}

	list, err := m.Next.List(ctx, params)
	if err != nil || list == nil {
		return list, err
	}
	if b, err := json.Marshal(list); err == nil {
		if err := m.VK.Do(ctx, m.VK.B().Set().Key(key).Value(valkey.BinaryString(b)).Ex(m.TTL).Build()).Error(); err != nil {
			logs.Warnf("clerkcache: set: %v", err)
		}
	}
	return list, nil
}

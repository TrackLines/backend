package integrations

import (
	"context"
	"sync"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	policysdk "github.com/policies2/go-sdk"
)

// UrgentPolicy asks policy2 whether a notification type is urgent (TL-882): {"type": "offer"} → true.
// What's urgent changes in policy2, without a deploy.
type UrgentPolicy struct {
	client *policysdk.ExecutionClient
	id     string
	mu     sync.Mutex
	cache  map[string]cached
}

type cached struct {
	urgent bool
	at     time.Time
}

// ponytail: a minute per type; a policy change shows within a minute, and a burst of notices asks once
const urgentCacheFor = time.Minute

// NewUrgentPolicy is nil without an API key: then nothing is urgent. baseURL "" is policy2's own.
func NewUrgentPolicy(apiKey, policyID, baseURL string) (*UrgentPolicy, error) {
	if apiKey == "" || policyID == "" {
		return nil, nil
	}
	c, err := policysdk.NewExecutionClient(policysdk.Config{APIKey: apiKey, Timeout: 5 * time.Second,
		Transport: policysdk.TransportConfig{Kind: policysdk.TransportKindREST, BaseURL: baseURL}})
	if err != nil {
		return nil, err
	}
	return &UrgentPolicy{client: c, id: policyID, cache: map[string]cached{}}, nil
}

// Urgent is policy2's answer for typ; not urgent if it can't be asked (a notice still goes, just quietly).
func (p *UrgentPolicy) Urgent(ctx context.Context, typ string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	c, ok := p.cache[typ]
	p.mu.Unlock()
	if ok && time.Since(c.at) < urgentCacheFor {
		return c.urgent
	}
	res, err := p.client.ExecutePolicy(ctx, policysdk.ExecutePolicyRequest{ID: p.id, Reference: policysdk.ReferenceBase, Data: map[string]any{"type": typ}})
	if err != nil {
		logs.Warnf("policy2: urgent %s: %v", typ, err)
		return false
	}
	p.mu.Lock()
	p.cache[typ] = cached{res.Result, time.Now()}
	p.mu.Unlock()
	return res.Result
}

// Package audit sends the auditor service what happened (TL-5, TL-666). Events are written to
// audit_outbox in the same transaction as the change (ticket stages, purges and plan changes by
// triggers; others with Record), and the Relay sends them on: a change never waits on the auditor, and
// nothing is lost while it's down.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// Event is one thing that happened, for Record. Data holds details, never secrets or ticket content.
type Event struct {
	Org        string
	Action     string // thing.happened, e.g. api_key.created
	TargetType string
	TargetID   string
	ProjectID  string
	Data       map[string]any
}

type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Record queues e in tx, done by whoever ctx's caller is.
func Record(ctx context.Context, tx Execer, e Event) error {
	data, err := json.Marshal(e.Data)
	if err != nil || e.Data == nil {
		data = []byte(`{}`)
	}
	var actor *string
	if a := auth.ActorID(ctx); a != "" {
		actor = &a
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_outbox (org_id, actor, actor_kind, action, target_type, target_id, project_id, data)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, '')::uuid, $8::jsonb)`,
		e.Org, actor, boards.ActorKind(ctx), e.Action, e.TargetType, e.TargetID, e.ProjectID, string(data))
	return err
}

const (
	batch = 500
	lease = time.Minute // a claimed batch isn't due again for this long (the send times out sooner)
	stale = time.Hour   // the oldest unsent event older than this is reported
)

// Relay sends due outbox rows to the auditor. Claims take rows FOR UPDATE SKIP LOCKED and push them a
// lease into the future, so several backends never send the same batch at once; one that dies mid-send
// is resent after the lease, which the auditor accepts again without storing it twice.
type Relay struct {
	DB     *pgxpool.Pool
	URL    string      // AUDIT_URL: the auditor on the private network
	Secret string      // AUDIT_SECRET (1Password audit_backend)
	On     func() bool // the "audit-service" flag: off, events queue and nothing is sent
	Client *http.Client
}

// Run sends every interval until ctx ends. Without a URL and secret it does nothing at all.
func (r Relay) Run(ctx context.Context, every time.Duration) {
	if r.URL == "" || r.Secret == "" {
		logs.Local().Logf("audit: AUDIT_URL or AUDIT_SECRET isn't set, so events stay queued")
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var warned time.Time
	for {
		if r.On == nil || r.On() {
			for {
				n, err := r.Tick(ctx)
				if err != nil && ctx.Err() == nil {
					logs.Errorf("audit: relay: %v", err)
				}
				if err != nil || n < batch {
					break // a full batch: there may be more waiting
				}
			}
			if age, err := r.oldest(ctx); err == nil && age > stale && time.Since(warned) > stale {
				logs.Errorf("audit: the oldest unsent event is %s old: is the auditor reachable?", age.Round(time.Minute))
				warned = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type outEvent struct {
	ID         string          `json:"id"`
	OrgID      string          `json:"org_id"`
	At         time.Time       `json:"at"`
	Actor      *string         `json:"actor"`
	ActorKind  string          `json:"actor_kind"`
	Action     string          `json:"action"`
	TargetType *string         `json:"target_type"`
	TargetID   *string         `json:"target_id"`
	ProjectID  *string         `json:"project_id"`
	Data       json.RawMessage `json:"data"`
	attempts   int
}

type result struct {
	Accepted []string `json:"accepted"`
	Rejected []struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

// Tick sends one batch of due events, oldest first, and returns how many it claimed. Accepted and
// rejected ones leave the outbox (a rejected one is reported: sending it again won't help); the rest wait
// longer each time.
func (r Relay) Tick(ctx context.Context) (int, error) {
	rows, err := r.DB.Query(ctx, `UPDATE audit_outbox o SET next_attempt_at = now() + $1::interval, attempts = o.attempts + 1
		WHERE o.id IN (SELECT id FROM audit_outbox WHERE next_attempt_at <= now() ORDER BY queued LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING o.id::text, o.queued, o.org_id, o.at, o.actor, o.actor_kind, o.action, o.target_type, o.target_id, o.project_id::text, o.data, o.attempts`,
		lease.String(), batch)
	if err != nil {
		return 0, err
	}
	type claimed struct {
		outEvent
		queued int64
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (claimed, error) {
		var c claimed
		err := row.Scan(&c.ID, &c.queued, &c.OrgID, &c.At, &c.Actor, &c.ActorKind, &c.Action, &c.TargetType, &c.TargetID, &c.ProjectID, &c.Data, &c.attempts)
		return c, err
	})
	if err != nil || len(got) == 0 {
		return 0, err
	}
	// RETURNING doesn't keep the subquery's order: send in the order they were queued
	evs := make([]outEvent, len(got))
	sort.Slice(got, func(i, j int) bool { return got[i].queued < got[j].queued })
	ids := make([]string, len(got))
	for i, c := range got {
		evs[i], ids[i] = c.outEvent, c.ID
	}
	res, err := r.send(ctx, evs)
	if err != nil {
		if _, berr := r.DB.Exec(ctx, `UPDATE audit_outbox SET next_attempt_at = now() + least(attempts * attempts, 60) * interval '1 minute'
			WHERE id = ANY($1::uuid[])`, ids); berr != nil {
			return len(got), fmt.Errorf("%w (and backing off: %v)", err, berr)
		}
		return len(got), err
	}
	done := append([]string{}, res.Accepted...)
	if len(res.Rejected) > 0 {
		var why []string
		for _, rj := range res.Rejected {
			done = append(done, rj.ID)
			why = append(why, rj.ID+": "+rj.Reason)
		}
		logs.Errorf("audit: the auditor refused %d event(s), dropped: %s", len(res.Rejected), strings.Join(why, "; "))
	}
	if _, err := r.DB.Exec(ctx, `DELETE FROM audit_outbox WHERE id = ANY($1::uuid[])`, done); err != nil {
		return len(got), err
	}
	return len(got), nil
}

func (r Relay) send(ctx context.Context, evs []outEvent) (result, error) {
	var out result
	body, err := json.Marshal(map[string]any{"events": evs})
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.URL, "/")+"/internal/events", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("auditor answered %s", res.Status)
	}
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// oldest is how long the oldest unsent event has waited.
func (r Relay) oldest(ctx context.Context) (time.Duration, error) {
	var at *time.Time
	if err := r.DB.QueryRow(ctx, `SELECT min(at) FROM audit_outbox`).Scan(&at); err != nil || at == nil {
		return 0, err
	}
	return time.Since(*at), nil
}

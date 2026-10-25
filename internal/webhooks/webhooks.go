// Package webhooks sends an org's events to its own https endpoints (TL-635): changes write an
// outbox row (Emit) in their own transaction where they have one, and a Worker delivers them,
// signed, with retries.
package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/secrets"
)

// Events people can subscribe to; "ping" (the test button) is sent but not subscribable.
var Events = []string{"ticket.created", "ticket.updated", "ticket.moved", "ticket.assigned", "ticket.done", "ticket.deleted",
	"comment.created", "sprint.started", "sprint.closed", "roadmap.update_published"}

const (
	Ping         = "ping"
	MaxEndpoints = 10
	PerPage      = 25
)

var (
	ErrNotFound  = errors.New("not found")
	ErrLimit     = httpx.Code("webhook_limit", "an organization can have up to 10 webhook endpoints")
	ErrBadURL    = httpx.Code("bad_webhook_url", "the URL must be https and not a private address")
	ErrBadEvents = httpx.Code("bad_webhook_events", "pick at least one known event")
	ErrPlan      = httpx.Code("webhooks_plan", "webhooks need the Pro or Enterprise plan")
)

// Querier is the pool, or the transaction making the change (so a rollback sends nothing).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Emit queues event for every enabled endpoint of org subscribed to it. data is only built (and
// marshalled) when there is one, so orgs without webhooks pay one indexed query.
func Emit(ctx context.Context, q Querier, org, event string, data func() (any, error)) error {
	err := emit(ctx, q, org, event, data)
	if err != nil && !httpx.Canceled(err) {
		logs.Errorf("webhooks: %s: %v", event, err)
	}
	return err
}

func emit(ctx context.Context, q Querier, org, event string, data func() (any, error)) error {
	var wanted bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_endpoints WHERE org_id = $1 AND enabled AND $2 = ANY (events))`,
		org, event).Scan(&wanted); err != nil || !wanted {
		return err
	}
	// webhooks are Pro and Enterprise: an org back on Free keeps its endpoints but gets nothing
	if err := q.QueryRow(ctx, `SELECT `+paidSQL("$1"), org).Scan(&wanted); err != nil || !wanted {
		return err
	}
	v, err := data()
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO webhook_outbox (endpoint_id, event, data)
		SELECT id, $2, $3::jsonb FROM webhook_endpoints WHERE org_id = $1 AND enabled AND $2 = ANY (events)`, org, event, string(b))
	return err
}

// paidSQL: org (an SQL expression) is on Pro or Enterprise, as billing.PlanOf decides it: a live
// subscription whose plan isn't free (a live one with no plan predates tiers: Enterprise).
func paidSQL(org string) string {
	return `EXISTS (SELECT 1 FROM org_billing ob WHERE ob.org_id = ` + org + `
		AND ob.stripe_status IN ('active', 'trialing') AND COALESCE(ob.plan, 'enterprise') <> 'free')`
}

// Endpoint is one of an org's webhook URLs; never its secret.
type Endpoint struct {
	ID                  string   `json:"id"`
	URL                 string   `json:"url"`
	Events              []string `json:"events"`
	Enabled             bool     `json:"enabled"`
	DisabledReason      *string  `json:"disabled_reason"` // failures | manual
	ConsecutiveFailures int      `json:"consecutive_failures"`
	SecretHint          string   `json:"secret_hint"`
	CreatedAt           string   `json:"created_at"`
}

// Delivery is one event sent (or to be sent) to an endpoint.
type Delivery struct {
	ID            string  `json:"id"`
	Event         string  `json:"event"`
	State         string  `json:"state"` // pending | ok | failed
	Attempts      int     `json:"attempts"`
	StatusCode    *int    `json:"status_code"`
	Error         *string `json:"error"`
	DurationMS    *int    `json:"duration_ms"`
	CreatedAt     string  `json:"created_at"`
	NextAttemptAt *string `json:"next_attempt_at"`
	RequestBody   string  `json:"request_body"`
	ResponseBody  *string `json:"response_body"`
}

const utc = `'YYYY-MM-DD"T"HH24:MI:SS"Z"'`

const endpointCols = `id::text, url, events, enabled, disabled_reason, consecutive_failures, secret_hint,
	to_char(created_at AT TIME ZONE 'UTC', ` + utc + `)`

func scanEndpoint(row pgx.Row) (Endpoint, error) {
	var e Endpoint
	err := row.Scan(&e.ID, &e.URL, &e.Events, &e.Enabled, &e.DisabledReason, &e.ConsecutiveFailures, &e.SecretHint, &e.CreatedAt)
	return e, err
}

// envelope is the request body: the same bytes for every attempt of a delivery.
func envelope(id, event, createdAt string, data []byte) []byte {
	b, _ := json.Marshal(struct {
		ID        string          `json:"id"`
		Event     string          `json:"event"`
		CreatedAt string          `json:"created_at"`
		Data      json.RawMessage `json:"data"`
	}{id, event, createdAt, data})
	return b
}

func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

type Store struct {
	DB  *pgxpool.Pool
	Box secrets.Box
}

func aad(org string) string { return "webhook|" + org }

// newSecret is a fresh signing secret, sealed for org, and its last 4 characters.
func (s Store) newSecret(org string) (plain string, sealed []byte, err error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	plain = "whsec_" + base64.RawURLEncoding.EncodeToString(raw)
	sealed, err = s.Box.SealWithAAD(plain, aad(org))
	return plain, sealed, err
}

func (s Store) List(ctx context.Context, org string) ([]Endpoint, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE org_id = $1 ORDER BY created_at, id`, org)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Endpoint, error) { return scanEndpoint(row) })
	if out == nil {
		out = []Endpoint{}
	}
	return out, err
}

// Create adds an endpoint (url and events already checked) and returns it with its plaintext secret.
func (s Store) Create(ctx context.Context, org, user, url string, events []string) (*Endpoint, string, error) {
	plain, sealed, err := s.newSecret(org)
	if err != nil {
		return nil, "", err
	}
	// ponytail: two admins adding the 10th at the same moment could make 11; an advisory lock if that matters
	e, err := scanEndpoint(s.DB.QueryRow(ctx, `INSERT INTO webhook_endpoints (org_id, url, events, secret, secret_hint, created_by)
		SELECT $1, $2, $3, $4, $5, $6 WHERE (SELECT count(*) FROM webhook_endpoints WHERE org_id = $1) < $7
		RETURNING `+endpointCols, org, url, events, sealed, plain[len(plain)-4:], user, MaxEndpoints))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrLimit
	}
	if err != nil {
		return nil, "", err
	}
	return &e, plain, nil
}

// Patch changes any of url, events and enabled. Turning it off is "manual"; on again starts afresh.
func (s Store) Patch(ctx context.Context, org, id string, url *string, events []string, enabled *bool) (*Endpoint, error) {
	e, err := scanEndpoint(s.DB.QueryRow(ctx, `UPDATE webhook_endpoints SET url = COALESCE($3, url), events = COALESCE($4, events),
			enabled = COALESCE($5, enabled),
			disabled_reason = CASE WHEN $5 IS NULL THEN disabled_reason WHEN $5 THEN NULL ELSE 'manual' END,
			consecutive_failures = CASE WHEN $5 THEN 0 ELSE consecutive_failures END
		WHERE id::text = $1 AND org_id = $2 RETURNING `+endpointCols, id, org, url, events, enabled))
	if err != nil {
		return nil, notFound(err)
	}
	return &e, nil
}

// Rotate gives the endpoint a new secret; the old one stops signing at once.
func (s Store) Rotate(ctx context.Context, org, id string) (string, error) {
	plain, sealed, err := s.newSecret(org)
	if err != nil {
		return "", err
	}
	tag, err := s.DB.Exec(ctx, `UPDATE webhook_endpoints SET secret = $3, secret_hint = $4 WHERE id::text = $1 AND org_id = $2`,
		id, org, sealed, plain[len(plain)-4:])
	if err != nil {
		return "", notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return plain, nil
}

func (s Store) Delete(ctx context.Context, org, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM webhook_endpoints WHERE id::text = $1 AND org_id = $2`, id, org)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Ping queues a test delivery to the endpoint.
func (s Store) Ping(ctx context.Context, org, id string) error {
	tag, err := s.DB.Exec(ctx, `INSERT INTO webhook_outbox (endpoint_id, event, data)
		SELECT id, $3, '{"message":"ping"}' FROM webhook_endpoints WHERE id::text = $1 AND org_id = $2`, id, org, Ping)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Redeliver sends a delivery's payload again: pending from now, with its retries afresh.
func (s Store) Redeliver(ctx context.Context, org, endpoint, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE webhook_outbox d SET state = 'pending', attempts = 0, next_attempt_at = now()
		FROM webhook_endpoints e WHERE d.id::text = $1 AND d.endpoint_id::text = $2 AND e.id = d.endpoint_id AND e.org_id = $3`, id, endpoint, org)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Deliveries is a page (from 1) of the endpoint's deliveries, newest first, and whether more follow.
func (s Store) Deliveries(ctx context.Context, org, endpoint string, page int) ([]Delivery, bool, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM webhook_endpoints WHERE id::text = $1 AND org_id = $2`, endpoint, org).Scan(&ok); err != nil {
		return nil, false, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text, event, data, state, attempts, status_code, error, duration_ms,
			to_char(created_at AT TIME ZONE 'UTC', `+utc+`), to_char(next_attempt_at AT TIME ZONE 'UTC', `+utc+`), response_body
		FROM webhook_outbox WHERE endpoint_id::text = $1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`,
		endpoint, PerPage+1, (page-1)*PerPage)
	if err != nil {
		return nil, false, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		var data []byte
		err := row.Scan(&d.ID, &d.Event, &data, &d.State, &d.Attempts, &d.StatusCode, &d.Error, &d.DurationMS, &d.CreatedAt, &d.NextAttemptAt, &d.ResponseBody)
		d.RequestBody = string(envelope(d.ID, d.Event, d.CreatedAt, data))
		return d, err
	})
	if err != nil {
		return nil, false, fmt.Errorf("deliveries: %w", err)
	}
	more := len(out) > PerPage
	if more {
		out = out[:PerPage]
	}
	if out == nil {
		out = []Delivery{}
	}
	return out, more, nil
}

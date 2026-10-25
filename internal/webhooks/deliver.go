package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/internal/safehttp"
	"github.com/tracklines/backend/internal/secrets"
)

// Backoff is the wait after each failed attempt; once it runs out the delivery has failed (6 attempts).
var Backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}

const (
	// DisableAfter consecutive failed attempts turns the endpoint off (disabled_reason "failures").
	DisableAfter = 5
	Keep         = 30 * 24 * time.Hour // deliveries older than this are deleted
	batch        = 20
	lease        = time.Minute // a claimed delivery isn't due again for this long (the send times out at 10s)
	maxResponse  = 2048
)

// Worker sends due deliveries. Claims take rows FOR UPDATE SKIP LOCKED and push them a lease into the
// future, so several instances never send the same one; one that dies mid-send is retried after the lease.
type Worker struct {
	DB     *pgxpool.Pool
	Box    secrets.Box
	Client *http.Client // nil: safehttp's (public addresses only, 10s)
	Notify notifications.Notifier
}

// Run sends due deliveries every interval, and clears out old ones hourly, until ctx ends.
func (w Worker) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var cleaned time.Time
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			logs.Errorf("webhooks: deliver: %v", err)
		}
		if time.Since(cleaned) > time.Hour {
			if _, err := w.Cleanup(ctx, time.Now()); err != nil && ctx.Err() == nil {
				logs.Errorf("webhooks: clean-up: %v", err)
			}
			cleaned = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type job struct {
	id, event, createdAt string
	data                 []byte
	attempts             int
	endpoint, org, url   string
	secret               []byte
}

// claim takes up to n due deliveries of enabled endpoints of orgs on Pro or Enterprise.
func (w Worker) claim(ctx context.Context, n int) ([]job, error) {
	rows, err := w.DB.Query(ctx, `UPDATE webhook_outbox d SET next_attempt_at = now() + $2::interval
		FROM webhook_endpoints e
		WHERE e.id = d.endpoint_id AND d.id IN (
			SELECT x.id FROM webhook_outbox x JOIN webhook_endpoints xe ON xe.id = x.endpoint_id
			WHERE x.state = 'pending' AND x.next_attempt_at <= now() AND xe.enabled
				AND `+paidSQL("xe.org_id")+` -- back on Free: they wait, unsent and not failed
			ORDER BY x.next_attempt_at LIMIT $1 FOR UPDATE OF x SKIP LOCKED)
		RETURNING d.id::text, d.event, to_char(d.created_at AT TIME ZONE 'UTC', `+utc+`), d.data, d.attempts,
			e.id::text, e.org_id, e.url, e.secret`, n, lease)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (job, error) {
		var j job
		return j, row.Scan(&j.id, &j.event, &j.createdAt, &j.data, &j.attempts, &j.endpoint, &j.org, &j.url, &j.secret)
	})
}

// Tick claims and sends due deliveries (in parallel, a batch at a time) until none are left; it
// returns how many it sent.
func (w Worker) Tick(ctx context.Context) (int, error) {
	sent := 0
	for {
		jobs, err := w.claim(ctx, batch)
		if err != nil || len(jobs) == 0 {
			return sent, err
		}
		var wg sync.WaitGroup
		for _, j := range jobs {
			wg.Go(func() {
				if err := w.record(ctx, j, w.send(ctx, j)); err != nil && ctx.Err() == nil {
					logs.Errorf("webhooks: delivery %s: %v", j.id, err)
				}
			})
		}
		wg.Wait()
		sent += len(jobs)
		if len(jobs) < batch {
			return sent, nil
		}
	}
}

type result struct {
	status   *int
	err      *string
	duration int
	response *string
}

func (r result) ok() bool {
	return r.err == nil && r.status != nil && *r.status >= 200 && *r.status < 300
}

// Sign is the TrackLines-Signature header for body sent at t: t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (w Worker) send(ctx context.Context, j job) result {
	fail := func(msg string) result { return result{err: &msg} }
	secret, err := w.Box.OpenWithAAD(j.secret, aad(j.org))
	if err != nil {
		return fail("the signing secret couldn't be read")
	}
	body := envelope(j.id, j.event, j.createdAt, j.data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url, bytes.NewReader(body))
	if err != nil {
		return fail(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TrackLin.es-Webhooks")
	req.Header.Set("TrackLines-Event", j.event)
	req.Header.Set("TrackLines-Delivery", j.id)
	req.Header.Set("TrackLines-Signature", Sign(secret, time.Now(), body))
	client := w.Client
	if client == nil {
		client = safehttp.Client(10 * time.Second)
	}
	start := time.Now()
	resp, err := client.Do(req)
	ms := int(time.Since(start).Milliseconds())
	if err != nil {
		r := fail(err.Error())
		r.duration = ms
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	text := strings.ReplaceAll(strings.ToValidUTF8(string(raw), "�"), "\x00", "") // Postgres text takes neither
	return result{status: &resp.StatusCode, duration: ms, response: &text}
}

// record stores an attempt: ok settles it and resets the endpoint's failures; a failure is retried
// on the Backoff schedule, and the DisableAfter-th failure in a row turns the endpoint off and tells
// the org's admins.
func (w Worker) record(ctx context.Context, j job, r result) error {
	attempts := j.attempts + 1
	if r.ok() {
		if _, err := w.DB.Exec(ctx, `UPDATE webhook_outbox SET state = 'ok', attempts = $2, status_code = $3, error = NULL,
			duration_ms = $4, response_body = $5, next_attempt_at = NULL WHERE id = $1`, j.id, attempts, r.status, r.duration, r.response); err != nil {
			return err
		}
		_, err := w.DB.Exec(ctx, `UPDATE webhook_endpoints SET consecutive_failures = 0 WHERE id = $1`, j.endpoint)
		return err
	}
	state, next := "failed", (*time.Duration)(nil)
	if attempts <= len(Backoff) {
		state, next = "pending", &Backoff[attempts-1]
	}
	if _, err := w.DB.Exec(ctx, `UPDATE webhook_outbox SET state = $2, attempts = $3, status_code = $4, error = $5,
			duration_ms = $6, response_body = $7, next_attempt_at = now() + $8::interval WHERE id = $1`,
		j.id, state, attempts, r.status, r.err, r.duration, r.response, next); err != nil {
		return err
	}
	var disabled bool
	if err := w.DB.QueryRow(ctx, `UPDATE webhook_endpoints SET consecutive_failures = consecutive_failures + 1,
			enabled = enabled AND consecutive_failures + 1 < $2,
			disabled_reason = CASE WHEN enabled AND consecutive_failures + 1 >= $2 THEN 'failures' ELSE disabled_reason END
		WHERE id = $1 RETURNING old.enabled AND NOT new.enabled`, j.endpoint, DisableAfter).Scan(&disabled); err != nil || !disabled {
		return err
	}
	admins, err := w.admins(ctx, j.org)
	if err != nil {
		return err
	}
	host := j.url
	if u, err := url.Parse(j.url); err == nil {
		host = u.Host
	}
	notifications.Send(ctx, w.Notify, j.org, admins, notifications.Event{Kind: "webhook_disabled",
		Text: "turned off the webhook to " + host + " after repeated failures"})
	return nil
}

func (w Worker) admins(ctx context.Context, org string) ([]string, error) {
	rows, err := w.DB.Query(ctx, `SELECT user_clerk_id FROM org_admins WHERE org_id = $1`, org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Cleanup deletes deliveries older than Keep.
func (w Worker) Cleanup(ctx context.Context, now time.Time) (int64, error) {
	tag, err := w.DB.Exec(ctx, `DELETE FROM webhook_outbox WHERE created_at < $1`, now.Add(-Keep))
	return tag.RowsAffected(), err
}

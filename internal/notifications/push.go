package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Multi tells every Notifier in turn (the inbox, then push), so callers send once.
type Multi []Notifier

func (m Multi) Notify(ctx context.Context, org string, to []string, e Event) error {
	var errs []error
	for _, n := range m {
		if n != nil {
			errs = append(errs, n.Notify(ctx, org, to, e))
		}
	}
	return errors.Join(errs...)
}

// PushConfig is the server's VAPID key pair (`npx web-push generate-vapid-keys`) and a contact.
type PushConfig struct {
	PublicKey  string
	PrivateKey string
	Subject    string // mailto:… or https://…, sent to push services
}

// Message is what a device shows.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"` // where clicking it goes
	Tag   string `json:"tag"` // one per notification kind and ticket, so repeats replace each other
}

type subscription struct {
	endpoint, p256dh, auth string
}

// Push sends notifications to people's devices with Web Push, in the background: a slow or failing
// push service never holds up the request that caused it. Devices that are gone (404/410) are dropped.
type Push struct {
	DB     *pgxpool.Pool
	Config PushConfig
	// People names actors in the message ("Ava assigned you …"); set after construction (tickets.People).
	People func(ctx context.Context, org string) ([]Person, error)
	send   func(ctx context.Context, s subscription, payload []byte) (int, error) // replaced in tests
	wg     sync.WaitGroup
}

func NewPush(db *pgxpool.Pool, cfg PushConfig) *Push {
	p := &Push{DB: db, Config: cfg}
	p.send = p.webpush
	return p
}

// Enabled: the server has VAPID keys, so devices can subscribe.
func (p *Push) Enabled() bool {
	return p != nil && p.Config.PublicKey != "" && p.Config.PrivateKey != "" && p.Config.Subject != ""
}

func (p *Push) webpush(ctx context.Context, s subscription, payload []byte) (int, error) {
	res, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{Endpoint: s.endpoint, Keys: webpush.Keys{P256dh: s.p256dh, Auth: s.auth}},
		&webpush.Options{Subscriber: p.Config.Subject, VAPIDPublicKey: p.Config.PublicKey, VAPIDPrivateKey: p.Config.PrivateKey, TTL: 24 * 60 * 60, Urgency: webpush.UrgencyNormal})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	return res.StatusCode, nil
}

// Notify queues the pushes and returns at once (same recipients as the inbox: people, once, never the actor).
func (p *Push) Notify(ctx context.Context, org string, to []string, e Event) error {
	if !p.Enabled() {
		return nil
	}
	seen := map[string]bool{e.Actor: true}
	var users []string
	for _, u := range to {
		if strings.HasPrefix(u, "user_") && !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
	}
	if len(users) == 0 {
		return nil
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := p.deliver(ctx, org, users, e); err != nil {
			logs.Warnf("push: %s: %v", e.Kind, err)
		}
	}()
	return nil
}

// Wait is for tests and shutdown: until queued pushes are sent.
func (p *Push) Wait() { p.wg.Wait() }

func (p *Push) message(ctx context.Context, org string, e Event) Message {
	actor := e.Actor
	if strings.HasPrefix(actor, "user_") {
		actor = "Someone"
		if p.People != nil {
			if people, err := p.People(ctx, org); err == nil {
				for _, x := range people {
					if x.ID == e.Actor && len(x.Names) > 0 {
						actor = x.Names[0]
					}
				}
			}
		}
	}
	m := Message{Title: "TrackLin.es", Body: actor + " " + e.Text, URL: "/", Tag: e.Kind}
	if e.TicketID != "" {
		var key string
		_ = p.DB.QueryRow(ctx, `SELECT coalesce(pr.key_prefix || '-' || t.number, t.id::text) FROM tickets t JOIN projects pr ON pr.id = t.project_id WHERE t.id::text = $1`, e.TicketID).Scan(&key)
		if key != "" {
			// the id, not the key: another of their orgs may have its own TL-12 (TL-276)
			m.URL, m.Tag = "/tickets/"+e.TicketID, e.Kind+":"+key
		}
	} else if e.BoardID != "" {
		m.URL, m.Tag = "/boards/"+e.BoardID, e.Kind+":"+e.BoardID
	}
	return m
}

func (p *Push) deliver(ctx context.Context, org string, users []string, e Event) error {
	rows, err := p.DB.Query(ctx, `SELECT endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = ANY($1)`, users)
	if err != nil {
		return err
	}
	var subs []subscription
	for rows.Next() {
		var s subscription
		if err := rows.Scan(&s.endpoint, &s.p256dh, &s.auth); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, s)
	}
	rows.Close()
	if len(subs) == 0 {
		return nil
	}
	payload, _ := json.Marshal(p.message(ctx, org, e))
	var errs []error
	for _, s := range subs {
		status, err := p.send(ctx, s, payload)
		switch {
		case err != nil:
			errs = append(errs, err)
		case status == http.StatusNotFound || status == http.StatusGone: // unsubscribed or expired
			_, _ = p.DB.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1`, s.endpoint)
		case status >= 400:
			errs = append(errs, errors.New("push service answered "+http.StatusText(status)))
		}
	}
	return errors.Join(errs...)
}

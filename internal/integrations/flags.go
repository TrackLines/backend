package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/tickets"
)

// flags.gg (TL-784): a ticket's feature flag follows it across the board. flags.gg keys everything by
// the ticket key, so we keep no flags.gg ids: the org's integration token (provider "flags"), each
// project's default agent, and which columns promote. See flags-gg/orchestrator's Tracklines guide.

const (
	Flags           = "flags"
	DefaultFlagsURL = "https://api.flags.gg"
)

// FlagEnvironment is one of an agent's environments, with the ticket's flag in it or not yet.
type FlagEnvironment struct {
	EnvironmentID string `json:"environment_id"`
	Name          string `json:"name"`
	Level         int    `json:"level"`
	Present       bool   `json:"present"`
	Enabled       bool   `json:"enabled"`
	Production    bool   `json:"production"`
}

// TicketFlag is a ticket's flag as flags.gg has it.
type TicketFlag struct {
	Ticket       string            `json:"ticket"`
	Name         string            `json:"name"`
	AgentID      string            `json:"agent_id"`
	Environments []FlagEnvironment `json:"environments"`
}

// inProduction: the flag has reached the agent's production environment.
func (f *TicketFlag) inProduction() bool {
	for _, e := range f.Environments {
		if e.Production && e.Present {
			return true
		}
	}
	return false
}

// FlagAgent is an agent (an application) the token can see, for the project's picker.
type FlagAgent struct {
	ProjectID    string `json:"project_id"`
	ProjectName  string `json:"project_name"`
	AgentID      string `json:"agent_id"`
	AgentName    string `json:"agent_name"`
	Environments []struct {
		EnvironmentID string `json:"environment_id"`
		Name          string `json:"name"`
		Level         int    `json:"level"`
		Production    bool   `json:"production"`
	} `json:"environments"`
}

// FlagsError is an answer flags.gg gave that retrying won't change (4xx), with its message.
type FlagsError struct {
	Status int
	Msg    string
}

func (e FlagsError) Error() string { return fmt.Sprintf("flags.gg: HTTP %d: %s", e.Status, e.Msg) }

// isFlags reports whether err is flags.gg answering status.
func isFlags(err error, status int) bool {
	var fe FlagsError
	return errors.As(err, &fe) && fe.Status == status
}

// FlagsAPI calls flags.gg with an org's token.
type FlagsAPI struct {
	Client *http.Client // nil: externalClient
}

func (a FlagsAPI) do(ctx context.Context, base, token, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.Client
	if client == nil {
		client = externalClient(10 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: flags.gg HTTP %d", errUnavailable, resp.StatusCode)
	case resp.StatusCode >= 400:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return FlagsError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func flagPath(key string) string { return "/integration/tickets/" + url.PathEscape(key) + "/flag" }

func (a FlagsAPI) Agents(ctx context.Context, base, token string) ([]FlagAgent, error) {
	var out struct {
		Agents []FlagAgent `json:"agents"`
	}
	err := a.do(ctx, base, token, http.MethodGet, "/integration/agents", nil, &out)
	if out.Agents == nil {
		out.Agents = []FlagAgent{}
	}
	return out.Agents, err
}

func (a FlagsAPI) Get(ctx context.Context, base, token, key string) (*TicketFlag, error) {
	var f TicketFlag
	return &f, a.do(ctx, base, token, http.MethodGet, flagPath(key), nil, &f)
}

func (a FlagsAPI) Create(ctx context.Context, base, token, key, agent, name string) (*TicketFlag, error) {
	var f TicketFlag
	return &f, a.do(ctx, base, token, http.MethodPut, flagPath(key), map[string]string{"agent_id": agent, "name": name}, &f)
}

func (a FlagsAPI) Set(ctx context.Context, base, token, key, env string, enabled bool) (*TicketFlag, error) {
	var f TicketFlag
	return &f, a.do(ctx, base, token, http.MethodPatch, flagPath(key), map[string]any{"environment_id": env, "enabled": enabled}, &f)
}

func (a FlagsAPI) Promote(ctx context.Context, base, token, key string, toProduction bool) (*TicketFlag, error) {
	path := flagPath(key) + "/promote"
	if toProduction {
		path += "?to=production"
	}
	var f TicketFlag
	return &f, a.do(ctx, base, token, http.MethodPost, path, nil, &f)
}

func (a FlagsAPI) Archive(ctx context.Context, base, token, key string) error {
	return a.do(ctx, base, token, http.MethodDelete, flagPath(key), nil, nil)
}

// FlagWorker sends flag_outbox jobs to flags.gg: one ticket's in order, retried with backoff while
// flags.gg is unreachable; an answer retrying won't change (a name clash, no agent) is shown on the ticket.
type FlagWorker struct {
	DB      *pgxpool.Pool
	Store   Store
	API     FlagsAPI
	Tickets tickets.Store
	Now     func() time.Time // nil: time.Now
}

const flagMaxAttempts = 10

func (w FlagWorker) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			logs.Errorf("flags worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type flagJob struct {
	id               int64
	org, ticket, act string
	attempts         int
}

// Tick sends the jobs that are due, at most one per ticket (its oldest), and says how many it handled.
func (w FlagWorker) Tick(ctx context.Context) (int, error) {
	var jobs []flagJob
	err := pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT o.id, o.org_id, o.ticket_id::text, o.action, o.attempts FROM flag_outbox o
			WHERE o.next_at <= now() AND NOT EXISTS (SELECT 1 FROM flag_outbox p WHERE p.ticket_id = o.ticket_id AND p.id < o.id)
			ORDER BY o.id LIMIT 20 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		jobs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (flagJob, error) {
			var j flagJob
			return j, r.Scan(&j.id, &j.org, &j.ticket, &j.act, &j.attempts)
		})
		if err != nil || len(jobs) == 0 {
			return err
		}
		ids := make([]int64, len(jobs))
		for i, j := range jobs {
			ids[i] = j.id
		}
		// leased: another worker leaves them alone while these are sent
		_, err = tx.Exec(ctx, `UPDATE flag_outbox SET next_at = now() + interval '2 minutes' WHERE id = ANY ($1)`, ids)
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		w.finish(ctx, j, w.send(ctx, j))
	}
	return len(jobs), nil
}

// finish: done (or not worth retrying) jobs go, with any error shown on the ticket; the rest wait longer each time.
func (w FlagWorker) finish(ctx context.Context, j flagJob, err error) {
	if err != nil && errors.Is(err, errUnavailable) && j.attempts+1 < flagMaxAttempts {
		wait := min(time.Duration(1<<j.attempts)*15*time.Second, time.Hour)
		if _, e := w.DB.Exec(ctx, `UPDATE flag_outbox SET attempts = attempts + 1, next_at = now() + $2::interval, last_error = $3 WHERE id = $1`,
			j.id, fmt.Sprintf("%d seconds", int(wait.Seconds())), err.Error()); e != nil {
			logs.Errorf("flags worker: retry %d: %v", j.id, e)
		}
		return
	}
	if err != nil {
		msg := err.Error()
		var fe FlagsError
		if errors.As(err, &fe) {
			msg = fe.Msg
		} else if errors.Is(err, errUnavailable) {
			msg = "flags.gg didn't answer: " + msg
		}
		if e := w.Tickets.FlagSent(ctx, j.ticket, "failed", msg); e != nil {
			logs.Errorf("flags worker: record error on %s: %v", j.ticket, e)
		}
	}
	if _, e := w.DB.Exec(ctx, `DELETE FROM flag_outbox WHERE id = $1`, j.id); e != nil {
		logs.Errorf("flags worker: delete %d: %v", j.id, e)
	}
}

// errNoAgent: neither the ticket nor its project says which flags.gg agent the flag goes in.
var errNoAgent = errors.New("pick a flags.gg agent for this project (Edit project → Integrations) before its tickets can have flags")

// send runs one job; nil when it's done.
func (w FlagWorker) send(ctx context.Context, j flagJob) error {
	base, token, err := w.Store.credentials(ctx, j.org, Flags)
	if errors.Is(err, ErrNotConnected) {
		return nil // disconnected since: nothing to send to
	}
	if err != nil {
		return err
	}
	f, err := w.Tickets.FlagTicketOf(ctx, j.org, j.ticket)
	if errors.Is(err, tickets.ErrNotFound) {
		return nil // the project went: its tickets' flags stay as they are in flags.gg
	}
	if err != nil {
		return err
	}
	create := func() error {
		if f.AgentID == "" {
			return errNoAgent
		}
		if _, err := w.API.Create(ctx, base, token, f.Key, f.AgentID, f.Name); err != nil {
			return err
		}
		return w.Tickets.FlagSent(ctx, f.ID, "created", "")
	}
	switch j.act {
	case "create":
		return create()
	case "archive":
		if err := w.API.Archive(ctx, base, token, f.Key); err != nil {
			return err
		}
		return w.Tickets.FlagSent(ctx, f.ID, "archived", "")
	case "promote", "promote_to_production":
		toProd := j.act == "promote_to_production"
		tf, err := w.API.Promote(ctx, base, token, f.Key, toProd)
		if isFlags(err, http.StatusNotFound) { // ticked after it reached the board: make it, then promote
			if err := create(); err != nil {
				return err
			}
			tf, err = w.API.Promote(ctx, base, token, f.Key, toProd)
		}
		if err != nil {
			return err
		}
		if err := w.Tickets.FlagSent(ctx, f.ID, "ok", ""); err != nil {
			return err
		}
		if tf.inProduction() && f.CleanupID == nil {
			now := time.Now
			if w.Now != nil {
				now = w.Now
			}
			due := now().UTC().AddDate(0, 0, f.CleanupDays).Format(time.DateOnly)
			_, err = w.Tickets.CreateFlagCleanup(ctx, j.org, f, due)
		}
		return err
	}
	return nil
}

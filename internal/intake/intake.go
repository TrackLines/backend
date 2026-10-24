// Package intake is a project's shareable request form: anyone with its unlisted link can send a bug
// report or feature request, without an account. Submissions wait for the team to triage them: promoted
// into an unassigned backlog ticket, or dismissed. Nothing about the project is shown to the sender.
package intake

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/tickets"
)

const (
	MaxTitle      = 200
	MaxDetails    = 5000
	MaxContact    = 200
	PerSenderHour = 5   // one sender's submissions per hour, per form
	PerProjectDay = 200 // all submissions per project per day
)

var (
	ErrClosed   = httpx.Code("form_closed", "this form isn't taking requests")
	ErrInvalid  = httpx.Code("bad_intake_request", "a title (up to 200 characters) and a type (bug or feature) are required")
	ErrTooMany  = httpx.Code("intake_rate_limited", "too many requests from you; please try again later")
	ErrNotFound = errors.New("not found")
	ErrDecided  = httpx.Code("already_triaged", "this request has already been triaged")
)

type Submission struct {
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	Type      string  `json:"type"`
	Title     string  `json:"title"`
	Details   string  `json:"details"`
	Contact   string  `json:"contact"`
	Status    string  `json:"status"`
	TicketID  *string `json:"ticket_id"`
	TicketKey *string `json:"ticket_key"`
	DecidedBy *string `json:"decided_by"`
	DecidedAt *string `json:"decided_at"`
	CreatedAt string  `json:"created_at"`
}

type Store struct{ DB *pgxpool.Pool }

func newToken() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Settings is a project's form: open (with its link token) or closed. ErrNotFound unless owner's project.
func (s Store) Settings(ctx context.Context, owner, project string) (token *string, err error) {
	err = s.DB.QueryRow(ctx, `SELECT intake_token FROM projects WHERE id::text = $1 AND owner_clerk_id = $2`, project, owner).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return token, err
}

// SetOpen opens the form (keeping its link, or a new one with rotate) or closes it (the link stops working).
func (s Store) SetOpen(ctx context.Context, owner, project string, open, rotate bool) (*string, error) {
	current, err := s.Settings(ctx, owner, project)
	if err != nil {
		return nil, err
	}
	var next *string
	if open {
		next = current
		if next == nil || rotate {
			t, err := newToken()
			if err != nil {
				return nil, err
			}
			next = &t
		}
	}
	_, err = s.DB.Exec(ctx, `UPDATE projects SET intake_token = $3 WHERE id::text = $1 AND owner_clerk_id = $2`, project, owner, next)
	return next, err
}

// Open reports whether token is a live form link.
func (s Store) Open(ctx context.Context, token string) bool {
	var ok bool
	_ = s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE intake_token = $1)`, token).Scan(&ok)
	return token != "" && ok
}

// Submit records a request sent through form token by sender (their address; only its hash is kept).
func (s Store) Submit(ctx context.Context, token, sender, typ, title, details, contact string) error {
	typ, title, details, contact = strings.TrimSpace(typ), strings.TrimSpace(title), strings.TrimSpace(details), strings.TrimSpace(contact)
	if (typ != "bug" && typ != "feature") || title == "" || len([]rune(title)) > MaxTitle || len([]rune(details)) > MaxDetails || len([]rune(contact)) > MaxContact {
		return ErrInvalid
	}
	sum := sha256.Sum256([]byte(token + "|" + sender))
	hash := hex.EncodeToString(sum[:])
	// check the caps and insert as one step: the project row is locked for the moment it takes, so a
	// burst of submissions queues up instead of all reading the count before any of them inserts.
	// (FOR NO KEY UPDATE: it doesn't hold up tickets being added to the project meanwhile.)
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var project string
		if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE intake_token = $1 FOR NO KEY UPDATE`, token).Scan(&project); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || token == "" {
				return ErrClosed
			}
			return err
		}
		var mine, today int
		if err := tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE sender = $2 AND created_at > now() - interval '1 hour'),
				count(*) FILTER (WHERE created_at > now() - interval '1 day')
			FROM intake_submissions WHERE project_id = $1 AND created_at > now() - interval '1 day'`, project, hash).Scan(&mine, &today); err != nil {
			return err
		}
		if mine >= PerSenderHour || today >= PerProjectDay {
			return ErrTooMany
		}
		_, err := tx.Exec(ctx, `INSERT INTO intake_submissions (project_id, type, title, details, contact, sender) VALUES ($1, $2, $3, $4, $5, $6)`,
			project, typ, title, details, contact, hash)
		return err
	})
}

const cols = `s.id, s.project_id, s.type, s.title, s.details, s.contact, s.status, s.ticket_id,
	(SELECT p2.key_prefix || '-' || t.number FROM tickets t JOIN projects p2 ON p2.id = t.project_id WHERE t.id = s.ticket_id),
	s.decided_by, to_char(s.decided_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(s.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Submission, error) {
	var x Submission
	err := row.Scan(&x.ID, &x.ProjectID, &x.Type, &x.Title, &x.Details, &x.Contact, &x.Status, &x.TicketID, &x.TicketKey, &x.DecidedBy, &x.DecidedAt, &x.CreatedAt)
	return x, err
}

// List is the project's submissions with status (pending by default), newest first.
func (s Store) List(ctx context.Context, owner, project, status string, limit int) ([]Submission, error) {
	if _, err := s.Settings(ctx, owner, project); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM intake_submissions s WHERE s.project_id::text = $1 AND s.status = $2
		ORDER BY s.created_at DESC, s.id DESC LIMIT $3`, project, status, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Submission, error) { return scan(row) })
	if out == nil {
		out = []Submission{}
	}
	return out, err
}

// pending locks submission id for a decision (the owner's, still pending).
func pending(ctx context.Context, tx pgx.Tx, owner, id string) (Submission, error) {
	x, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM intake_submissions s JOIN projects p ON p.id = s.project_id
		WHERE s.id::text = $1 AND p.owner_clerk_id = $2 FOR UPDATE OF s`, id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return x, ErrNotFound
	}
	if err == nil && x.Status != "pending" {
		return x, ErrDecided
	}
	return x, err
}

// Promote turns a pending submission into an unassigned backlog ticket, made by actor.
func (s Store) Promote(ctx context.Context, owner, actor, id string) (Submission, error) {
	var out Submission
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		x, err := pending(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		desc := x.Details
		note := "_Sent through the project's request form"
		if x.Contact != "" {
			note += " by `" + strings.ReplaceAll(x.Contact, "`", "'") + "`" // code: an anonymous sender's text never renders as a link (audit)
		}
		note += "._"
		if desc != "" {
			desc += "\n\n"
		}
		// ponytail: the ticket is made outside this tx (tickets.Store has no tx variant); the row lock still
		// stops a double promote, and a failure after it leaves a normal backlog ticket, not a lost request
		// made as the promoter, so its history starts with them (TL-75), not the organization
		t, err := tickets.Store{DB: s.DB}.CreateBacklogAs(ctx, owner, actor, x.ProjectID, x.Type, x.Title, desc+note)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE intake_submissions SET status = 'promoted', ticket_id = $2, decided_by = $3, decided_at = now() WHERE id = $1`, x.ID, t.ID, actor); err != nil {
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM intake_submissions s WHERE s.id = $1`, x.ID))
		return err
	})
	return out, err
}

// Dismiss closes a pending submission without making a ticket.
func (s Store) Dismiss(ctx context.Context, owner, actor, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		x, err := pending(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE intake_submissions SET status = 'dismissed', decided_by = $2, decided_at = now() WHERE id = $1`, x.ID, actor)
		return err
	})
}

package attachments

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

var (
	ErrNotFound = errors.New("not found")
	ErrBadFile  = errors.New("file must be an UploadThing upload (https, *.ufs.sh or utfs.io) with a key and name")
	ErrDone     = errors.New("ticket is done — move it out of Done to change its attachments")
)

type Attachment struct {
	ID          string `json:"id"`
	TicketID    string `json:"ticket_id"`
	Key         string `json:"key"`
	URL         string `json:"url"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
}

type Store struct{ DB *pgxpool.Pool }

const cols = `a.id, a.ticket_id, a.file_key, a.url, a.name, a.size_bytes, a.content_type, a.created_by,
	to_char(a.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.TicketID, &a.Key, &a.URL, &a.Name, &a.Size, &a.ContentType, &a.CreatedBy, &a.CreatedAt)
	return a, err
}

func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// ValidUploadThingURL accepts only files hosted by UploadThing, so a ticket can't be made to
// point at arbitrary links.
func ValidUploadThingURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "utfs.io" || strings.HasSuffix(h, ".ufs.sh")
}

// Create records an uploaded file on the owner's ticket.
func (s Store) Create(ctx context.Context, owner, actor, ticketID string, a Attachment) (*Attachment, error) {
	if a.Key == "" || strings.TrimSpace(a.Name) == "" || !ValidUploadThingURL(a.URL) {
		return nil, ErrBadFile
	}
	var done bool
	if err := s.DB.QueryRow(ctx, `SELECT `+boards.TicketDoneSQL+` FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2`, ticketID, owner).Scan(&done); err != nil {
		return nil, notFound(err)
	}
	if done {
		return nil, ErrDone
	}
	out, err := scan(s.DB.QueryRow(ctx, `INSERT INTO ticket_attachments AS a (ticket_id, file_key, url, name, size_bytes, content_type, created_by)
		SELECT t.id, $3, $4, $5, $6, $7, $8 FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2
		ON CONFLICT (file_key) DO NOTHING
		RETURNING `+cols, ticketID, owner, a.Key, a.URL, a.Name, max(a.Size, 0), a.ContentType, actor))
	if err != nil {
		return nil, notFound(err) // also: key already recorded
	}
	return &out, nil
}

// List returns the owner's ticket attachments, oldest first.
func (s Store) List(ctx context.Context, owner, ticketID string) ([]Attachment, error) {
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2`, ticketID, owner).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM ticket_attachments a WHERE a.ticket_id = $1 ORDER BY a.created_at`, ticketID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Attachment, error) { return scan(row) })
	if out == nil {
		out = []Attachment{}
	}
	return out, err
}

// Delete removes the owner's attachment record and returns its UploadThing file key.
// Attachments on a done ticket can't be removed either (ErrDone).
func (s Store) Delete(ctx context.Context, owner, id string) (string, error) {
	var key string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var done bool
		if err := tx.QueryRow(ctx, `SELECT `+boards.TicketDoneSQL+` FROM ticket_attachments a
			JOIN tickets t ON t.id = a.ticket_id JOIN projects p ON p.id = t.project_id
			WHERE a.id = $1 AND p.owner_clerk_id = $2 FOR UPDATE OF a`, id, owner).Scan(&done); err != nil {
			return notFound(err)
		}
		if done {
			return ErrDone
		}
		return tx.QueryRow(ctx, `DELETE FROM ticket_attachments WHERE id = $1 RETURNING file_key`, id).Scan(&key)
	})
	return key, err
}

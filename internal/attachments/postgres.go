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
	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrNotFound = errors.New("not found")
	ErrBadFile  = httpx.Code("bad_file", "file must be an UploadThing upload (https, *.ufs.sh or utfs.io) with a key and name")
	ErrDone     = httpx.Code("done_ticket_files", "ticket is done — move it out of Done to change its attachments")
	ErrTooMany  = httpx.Code("too_many_files", "this ticket already has as many files as it can hold: remove one to add another")
	ErrTooBig   = httpx.Code("file_too_big", "files can be up to 10 MB")
	ErrCantKeep = httpx.Code("cant_keep_file", "only an organization admin or whoever uploaded it can keep this file")
	// TL-659
	ErrCantRemove = httpx.Code("cant_remove_file", "only an organization admin or whoever uploaded it can remove this file")
	ErrBadUpload  = httpx.Code("bad_upload", "a file needs a name and a valid content type")
)

type Attachment struct {
	ID          string  `json:"id"`
	TicketID    string  `json:"ticket_id"`
	Key         string  `json:"key"`
	URL         string  `json:"url"`
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	ContentType string  `json:"content_type"`
	CreatedBy   string  `json:"created_by"`
	CreatedAt   string  `json:"created_at"`
	DeleteOn    *string `json:"delete_on,omitempty"` // warned about (TL-259): the day it goes unless kept
}

type Store struct{ DB *pgxpool.Pool }

const cols = `a.id, a.ticket_id, a.file_key, a.url, a.name, a.size_bytes, a.content_type, a.created_by,
	to_char(a.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	CASE WHEN a.warned_at IS NULL THEN NULL ELSE to_char(` + deleteOnSQL + ` AT TIME ZONE 'UTC', 'YYYY-MM-DD') END`

func scan(row pgx.Row) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.TicketID, &a.Key, &a.URL, &a.Name, &a.Size, &a.ContentType, &a.CreatedBy, &a.CreatedAt, &a.DeleteOn)
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

// ValidUploadThingFile ensures the URL identifies the same UploadThing file as the key
// that will be sent to its delete API. This prevents a caller from attaching a trusted URL
// while naming a different file key for deletion.
func ValidUploadThingFile(raw, key string) bool {
	if key == "" || !ValidUploadThingURL(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.EscapedPath(), "/f/"+url.PathEscape(key))
}

// MaxPerTicket and MaxFileBytes bound a ticket's attachments (TL-253); raise them here (and in the
// frontend's lib/attachment-limits.ts).
const (
	MaxPerTicket = 10
	MaxFileBytes = 10 << 20
)

// room: whether the owner's ticket can take another file: it exists in their org, isn't done and isn't
// full. With lock, the ticket row stays locked until q's transaction ends, so adds to one ticket queue;
// it's locked first and counted after, so the count sees what the add before this one committed.
func room(ctx context.Context, q pgx.Tx, owner, ticketID string, lock bool) error {
	if lock {
		if _, err := q.Exec(ctx, `SELECT 1 FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = $1 AND p.owner_clerk_id = $2 FOR UPDATE OF t`, ticketID, owner); err != nil {
			return notFound(err)
		}
	}
	sql := `SELECT ` + boards.TicketDoneSQL + `, (SELECT count(*) FROM ticket_attachments a WHERE a.ticket_id = t.id)
		FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2`
	var done bool
	var count int
	if err := q.QueryRow(ctx, sql, ticketID, owner).Scan(&done, &count); err != nil {
		return notFound(err)
	}
	if done {
		return ErrDone
	}
	if count >= MaxPerTicket {
		return ErrTooMany
	}
	return nil
}

// CanAdd checks a file would be taken before it's uploaded (Upload); Create checks again, locked.
func (s Store) CanAdd(ctx context.Context, owner, ticketID string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error { return room(ctx, tx, owner, ticketID, false) })
}

// Create records an uploaded file on the owner's ticket. The ticket is locked while it's counted, so
// files added at the same moment can't take it past MaxPerTicket.
func (s Store) Create(ctx context.Context, owner, actor, ticketID string, a Attachment) (*Attachment, error) {
	if strings.TrimSpace(a.Name) == "" || !ValidUploadThingFile(a.URL, a.Key) {
		return nil, ErrBadFile
	}
	if a.Size > MaxFileBytes {
		return nil, ErrTooBig
	}
	var out Attachment
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := room(ctx, tx, owner, ticketID, true); err != nil {
			return err
		}
		var err error
		out, err = scan(tx.QueryRow(ctx, `INSERT INTO ticket_attachments AS a (ticket_id, file_key, url, name, size_bytes, content_type, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (file_key) DO NOTHING
			RETURNING `+cols, ticketID, a.Key, a.URL, a.Name, max(a.Size, 0), a.ContentType, actor))
		return notFound(err) // also: key already recorded
	})
	if err != nil {
		return nil, err
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

// Delete removes the owner's attachment record and returns its UploadThing file key, when may
// allows it for whoever uploaded it (ErrCantRemove otherwise, TL-659).
// Attachments on a done ticket can't be removed either (ErrDone).
func (s Store) Delete(ctx context.Context, owner, id string, may func(uploader string) bool) (string, error) {
	var key string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var done bool
		var uploader string
		if err := tx.QueryRow(ctx, `SELECT `+boards.TicketDoneSQL+`, a.created_by FROM ticket_attachments a
			JOIN tickets t ON t.id = a.ticket_id JOIN projects p ON p.id = t.project_id
			WHERE a.id = $1 AND p.owner_clerk_id = $2 FOR UPDATE OF a`, id, owner).Scan(&done, &uploader); err != nil {
			return notFound(err)
		}
		if done {
			return ErrDone
		}
		if !may(uploader) {
			return ErrCantRemove
		}
		return tx.QueryRow(ctx, `DELETE FROM ticket_attachments WHERE id = $1 RETURNING file_key`, id).Scan(&key)
	})
	return key, err
}

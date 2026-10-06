package comments

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MaxBody = 10000

var (
	ErrNotFound  = errors.New("not found")
	ErrBadParent = errors.New("parent_comment_id must be a comment on the same ticket")
	ErrBadBody   = errors.New("comment can't be empty or longer than 10000 characters")
)

// Comment is one message in a ticket's conversation. ParentID is set on replies.
type Comment struct {
	ID        string  `json:"id"`
	TicketID  string  `json:"ticket_id"`
	ParentID  *string `json:"parent_comment_id"`
	Body      string  `json:"body"`
	Author    string  `json:"author"` // auth.ActorID: API key name, or the Clerk user id
	CreatedAt string  `json:"created_at"`
}

type Store struct{ DB *pgxpool.Pool }

const cols = `c.id, c.ticket_id, c.parent_comment_id, c.body, c.author,
	to_char(c.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`

func scan(row pgx.Row) (Comment, error) {
	var c Comment
	err := row.Scan(&c.ID, &c.TicketID, &c.ParentID, &c.Body, &c.Author, &c.CreatedAt)
	return c, err
}

func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// owns reports ErrNotFound unless the ticket is in one of owner's projects.
func (s Store) owns(ctx context.Context, owner, ticketID string) error {
	var ok bool
	return notFound(s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2`, ticketID, owner).Scan(&ok))
}

// List returns the ticket's comments oldest first (replies interleaved by time; clients nest by parent).
func (s Store) List(ctx context.Context, owner, ticketID string) ([]Comment, error) {
	if err := s.owns(ctx, owner, ticketID); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM ticket_comments c WHERE c.ticket_id = $1 ORDER BY c.created_at, c.id`, ticketID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Comment, error) { return scan(row) })
	if out == nil {
		out = []Comment{}
	}
	return out, err
}

// Create adds a comment (or a reply when parentID is set) by author on the owner's ticket.
func (s Store) Create(ctx context.Context, owner, author, ticketID, body string, parentID *string) (*Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > MaxBody {
		return nil, ErrBadBody
	}
	if parentID != nil && *parentID == "" {
		parentID = nil
	}
	if err := s.owns(ctx, owner, ticketID); err != nil {
		return nil, err
	}
	c, err := scan(s.DB.QueryRow(ctx, `INSERT INTO ticket_comments AS c (ticket_id, parent_comment_id, body, author)
		VALUES ($1, $2, $3, $4) RETURNING `+cols, ticketID, parentID, body, author))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && (pg.Code == "23503" || pg.Code == "22P02") { // parent not on this ticket / not a uuid
		return nil, ErrBadParent
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

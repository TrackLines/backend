package bugfixesTickets

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BUGFIXES_KEY_PREFIX is the prefix Bugfixes API keys use so the auth middleware
// can tell them from Clerk JWTs (tl_) and from other agent keys.
const BUGFIXES_KEY_PREFIX = "bf_"

var (
	ErrBadBoard  = errors.New("bad board")
	ErrBadColumn = errors.New("bad column")
	ErrNotFound  = errors.New("not found")
)

type Ticket struct {
	ID        string `json:"id"`
	BoardID   string `json:"board_id"`
	ColumnID  string `json:"column_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Priority  string `json:"priority"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

type Store struct{ DB *pgxpool.Pool }

// Owner resolves a bugfixes key to its owner + name, stamping last_used_at.
// Lookup is by sha256 hash (same pattern as apikeys.Owner).
func (s Store) Owner(ctx context.Context, plain string) (string, string, error) {
	h := sha256.Sum256([]byte(plain))
	var owner, name string
	err := s.DB.QueryRow(ctx, `
		UPDATE api_keys SET last_used_at = now()
		WHERE hash = $1 AND revoked_at IS NULL
		RETURNING owner_clerk_id, name
	`, h[:]).Scan(&owner, &name)
	if err != nil {
		return "", "", ErrNotFound
	}
	return owner, name, nil
}

// Create inserts a ticket on behalf of the bugfixes agent identified by agentName.
func (s Store) Create(ctx context.Context, boardID, columnID, title, body, priority, agentName string) (*Ticket, error) {
	// Verify the column exists and belongs to the requested board.
	var colBoardID string
	err := s.DB.QueryRow(ctx, `SELECT board_id FROM columns WHERE id = $1`, columnID).Scan(&colBoardID)
	if err != nil {
		return nil, ErrBadColumn
	}
	if colBoardID != boardID {
		return nil, ErrBadColumn
	}

	t := &Ticket{
		BoardID:   boardID,
		ColumnID:  columnID,
		Title:     title,
		Body:      body,
		Priority:  priority,
		CreatedBy: agentName,
	}

	err = s.DB.QueryRow(ctx, `
		INSERT INTO tickets (board_id, column_id, title, description, position, created_by)
		SELECT $1, $2, $3, $4, COALESCE(MAX(position), -1) + 1, $5
		FROM tickets WHERE column_id = $2
		RETURNING id, to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	`, boardID, columnID, title, body, agentName).Scan(&t.ID, &t.CreatedAt)

	if err != nil {
		if strings.Contains(err.Error(), "foreign key") || strings.Contains(err.Error(), "violates") {
			return nil, ErrBadBoard
		}
		return nil, err
	}
	return t, nil
}

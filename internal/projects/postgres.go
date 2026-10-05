package projects

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/roadmaps"
)

var (
	ErrNotFound = errors.New("project not found")
	ErrLimit    = errors.New("project limit reached for plan")
)

// Project groups a team's boards (one per team) and its roadmaps.
type Project struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
	Boards      []boards.Board     `json:"boards,omitempty"`
	Roadmaps    []roadmaps.Roadmap `json:"roadmaps,omitempty"`
}

type Store struct{ DB *pgxpool.Pool }

const cols = `id, name, description,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	return p, notFound(err)
}

// notFound maps "no rows" and malformed uuids (22P02) to ErrNotFound.
func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

// Create inserts a project. limit < 0 means unlimited; otherwise ErrLimit when the owner
// already has limit projects (checked under a per-owner lock so concurrent creates
// can't both slip past the free tier).
func (s Store) Create(ctx context.Context, owner, name, desc string, limit int) (*Project, error) {
	var p Project
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if limit >= 0 {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('projects:' || $1))`, owner); err != nil {
				return err
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM projects WHERE owner_clerk_id = $1`, owner).Scan(&n); err != nil {
				return err
			}
			if n >= limit {
				return ErrLimit
			}
		}
		var err error
		p, err = scan(tx.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, description)
			VALUES ($1, $2, $3) RETURNING `+cols, owner, name, desc))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s Store) List(ctx context.Context, owner string) ([]Project, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM projects WHERE owner_clerk_id = $1 ORDER BY created_at`, owner)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Project, error) { return scan(row) })
	if out == nil {
		out = []Project{}
	}
	return out, err
}

// Get returns the owner's project with its boards and roadmaps (no columns/items).
func (s Store) Get(ctx context.Context, id, owner string) (*Project, error) {
	p, err := scan(s.DB.QueryRow(ctx, `SELECT `+cols+` FROM projects WHERE id = $1 AND owner_clerk_id = $2`, id, owner))
	if err != nil {
		return nil, err
	}
	if p.Boards, err = (boards.Store{DB: s.DB}).ListByProject(ctx, id); err != nil {
		return nil, err
	}
	if p.Roadmaps, err = (roadmaps.Store{DB: s.DB}).ListByProject(ctx, id); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s Store) Update(ctx context.Context, id, owner, name, desc string) error {
	return exec(s.DB.Exec(ctx, `UPDATE projects SET name = $3, description = $4, updated_at = now()
		WHERE id = $1 AND owner_clerk_id = $2`, id, owner, name, desc))
}

// Delete removes the project and (cascade) its boards, tickets and roadmaps.
func (s Store) Delete(ctx context.Context, id, owner string) error {
	return exec(s.DB.Exec(ctx, `DELETE FROM projects WHERE id = $1 AND owner_clerk_id = $2`, id, owner))
}

func exec(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

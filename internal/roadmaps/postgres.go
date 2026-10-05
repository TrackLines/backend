package roadmaps

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound          = errors.New("roadmap not found")
	ErrInvalidVisibility = errors.New("visibility must be public, login_only or team")
)

const (
	Public    = "public"
	LoginOnly = "login_only"
	Team      = "team"
)

type Item struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	TargetDate  *string `json:"target_date"` // YYYY-MM-DD or null
	Position    int     `json:"position"`
}

type Roadmap struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	OwnerClerkID string `json:"-"` // never exposed: public roadmaps would leak Clerk ids
	Title        string `json:"title"`
	Description  string `json:"description"`
	Visibility   string `json:"visibility"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Items        []Item `json:"items,omitempty"`
}

type Store struct{ DB *pgxpool.Pool }

const cols = `id, project_id, owner_clerk_id, title, description, visibility::text,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scan(row pgx.Row) (Roadmap, error) {
	var r Roadmap
	err := row.Scan(&r.ID, &r.ProjectID, &r.OwnerClerkID, &r.Title, &r.Description, &r.Visibility, &r.CreatedAt, &r.UpdatedAt)
	return r, notFound(err)
}

// notFound maps "no rows" and malformed uuids (22P02) to ErrNotFound.
func notFound(err error) error {
	var pg *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pg) && pg.Code == "22P02") {
		return ErrNotFound
	}
	return err
}

func validVisibility(v string) bool { return v == Public || v == LoginOnly || v == Team }

// Get returns a roadmap with its items; access checks are the caller's job.
func (s Store) Get(ctx context.Context, id string) (*Roadmap, error) {
	r, err := scan(s.DB.QueryRow(ctx, `SELECT `+cols+` FROM roadmaps WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id, title, description, target_date::text, position
		FROM roadmap_items WHERE roadmap_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	r.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
		var i Item
		err := row.Scan(&i.ID, &i.Title, &i.Description, &i.TargetDate, &i.Position)
		return i, err
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s Store) list(ctx context.Context, where string, arg any) ([]Roadmap, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+` FROM roadmaps WHERE `+where+` ORDER BY updated_at DESC`, arg)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Roadmap, error) { return scan(row) })
	if out == nil {
		out = []Roadmap{}
	}
	return out, err
}

func (s Store) ListByOwner(ctx context.Context, owner string) ([]Roadmap, error) {
	return s.list(ctx, `owner_clerk_id = $1`, owner)
}

// ListByProject lists a project's roadmaps (without items); callers check ownership.
func (s Store) ListByProject(ctx context.Context, projectID string) ([]Roadmap, error) {
	return s.list(ctx, `project_id = $1`, projectID)
}

// Create adds a roadmap to the owner's project; ErrNotFound when the project isn't theirs.
func (s Store) Create(ctx context.Context, owner, projectID, title, desc, visibility string) (*Roadmap, error) {
	if !validVisibility(visibility) {
		return nil, ErrInvalidVisibility
	}
	r, err := scan(s.DB.QueryRow(ctx, `INSERT INTO roadmaps (project_id, owner_clerk_id, title, description, visibility)
		SELECT id, owner_clerk_id, $3, $4, $5 FROM projects WHERE id = $1 AND owner_clerk_id = $2
		RETURNING `+cols, projectID, owner, title, desc, visibility))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Update edits a roadmap the owner holds; ErrNotFound covers "not yours" too.
func (s Store) Update(ctx context.Context, id, owner, title, desc, visibility string) error {
	if !validVisibility(visibility) {
		return ErrInvalidVisibility
	}
	return exec(s.DB.Exec(ctx, `UPDATE roadmaps SET title = $3, description = $4, visibility = $5, updated_at = now()
		WHERE id = $1 AND owner_clerk_id = $2`, id, owner, title, desc, visibility))
}

func (s Store) Delete(ctx context.Context, id, owner string) error {
	return exec(s.DB.Exec(ctx, `DELETE FROM roadmaps WHERE id = $1 AND owner_clerk_id = $2`, id, owner))
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

// ReplaceItems swaps the whole item list in one tx; slice order becomes position.
func (s Store) ReplaceItems(ctx context.Context, id, owner string, items []Item) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := exec(tx.Exec(ctx, `UPDATE roadmaps SET updated_at = now()
			WHERE id = $1 AND owner_clerk_id = $2`, id, owner)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM roadmap_items WHERE roadmap_id = $1`, id); err != nil {
			return err
		}
		for pos, i := range items {
			if _, err := tx.Exec(ctx, `INSERT INTO roadmap_items (roadmap_id, title, description, target_date, position)
				VALUES ($1, $2, $3, $4::date, $5)`, id, i.Title, i.Description, i.TargetDate, pos); err != nil {
				return err
			}
		}
		return nil
	})
}

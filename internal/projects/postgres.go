package projects

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
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
	KeyPrefix   string             `json:"key_prefix"` // tickets are <prefix>-<number>, e.g. TL-123
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
	ArchivedAt  *string            `json:"archived_at"` // archived: off the project lists, history kept (TL-907)
	Deletable   bool               `json:"deletable"`   // never used (no tickets, no sprints): can be deleted instead of archived
	Boards      []boards.Board     `json:"boards,omitempty"`
	Roadmaps    []roadmaps.Roadmap `json:"roadmaps,omitempty"`
	Stats       *Stats             `json:"stats,omitempty"` // List only
}

// Stats summarises a project for the projects page. Open and Done count board tickets;
// Urgent is open urgent tickets on boards or in the backlog.
type Stats struct {
	Boards   int    `json:"boards"`
	Roadmaps int    `json:"roadmaps"`
	Open     int    `json:"open"`
	Done     int    `json:"done"`
	Backlog  int    `json:"backlog"`
	Urgent   int    `json:"urgent"`
	Active   string `json:"active"` // latest ticket change, or the project's own
}

type Store struct{ DB *pgxpool.Pool }

const cols = `id, name, description, key_prefix,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(archived_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ` + unusedSQL

// unusedSQL: a project nobody has worked in yet. Only those can be deleted; used ones are archived.
const unusedSQL = `NOT EXISTS (SELECT 1 FROM tickets t WHERE t.project_id = projects.id)
	AND NOT EXISTS (SELECT 1 FROM sprints s JOIN boards b ON b.id = s.board_id WHERE b.project_id = projects.id)`

func scan(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.KeyPrefix, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt, &p.Deletable)
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

// List returns the owner's active projects, or only the archived ones.
func (s Store) List(ctx context.Context, owner string, archived bool) ([]Project, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+cols+`,
		(SELECT count(*) FROM boards b WHERE b.project_id = projects.id AND b.archived_at IS NULL),
		(SELECT count(*) FROM roadmaps r WHERE r.project_id = projects.id),
		count(*) FILTER (WHERE t.board_id IS NOT NULL AND NOT t.done),
		count(*) FILTER (WHERE t.done),
		count(t.tid) FILTER (WHERE t.board_id IS NULL AND NOT t.done), -- open backlog only (resolved ones are done); count(t.tid): a project with no tickets joins one all-NULL row
		count(*) FILTER (WHERE t.priority = 'urgent' AND NOT t.done),
		to_char(greatest(projects.updated_at, max(t.changed)) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM projects
		LEFT JOIN LATERAL (SELECT t.id AS tid, t.board_id, t.priority, t.updated_at AS changed, `+boards.TicketDoneSQL+` AS done
			FROM tickets t WHERE t.project_id = projects.id) t ON true
		WHERE owner_clerk_id = $1 AND (archived_at IS NOT NULL) = $2 GROUP BY projects.id ORDER BY created_at`, owner, archived)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Project, error) {
		p, st := Project{}, Stats{}
		err := row.Scan(&p.ID, &p.Name, &p.Description, &p.KeyPrefix, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt, &p.Deletable,
			&st.Boards, &st.Roadmaps, &st.Open, &st.Done, &st.Backlog, &st.Urgent, &st.Active)
		p.Stats = &st
		return p, err
	})
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

var (
	ErrBadKeyPrefix   = httpx.Code("bad_key_prefix", "key prefix must be 2–6 capital letters or digits, starting with a letter")
	ErrKeyPrefixTaken = httpx.Code("key_prefix_taken", "another project in this organization already uses that key prefix")
)

// SetKeyPrefix renames the project's ticket keys (TL-123 → TRK-123). Old keys in branch names or
// PR titles stop matching. ponytail: no alias history; add one if renames turn out common.
func (s Store) SetKeyPrefix(ctx context.Context, id, owner, prefix string) error {
	if !keyPrefix.MatchString(prefix) {
		return ErrBadKeyPrefix
	}
	err := exec(s.DB.Exec(ctx, `UPDATE projects SET key_prefix = $3, updated_at = now() WHERE id = $1 AND owner_clerk_id = $2`, id, owner, prefix))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrKeyPrefixTaken
	}
	return err
}

var keyPrefix = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,5}$`) // same as projects_key_prefix_format

var ErrProjectUsed = httpx.Code("project_used", "this project has been used; archive it instead, so its history is kept")

// Delete removes a never-used project (and, by cascade, its empty boards and roadmaps); ErrProjectUsed otherwise.
func (s Store) Delete(ctx context.Context, id, owner string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM projects WHERE id = $1 AND owner_clerk_id = $2 AND `+unusedSQL, id, owner)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, err := s.Get(ctx, id, owner); err != nil {
		return err
	}
	return ErrProjectUsed
}

// Archive takes a project off the project lists, history kept. Its boards' open sprints close with no
// successor and their planned sprints go (tickets back to the backlog unplanned), as when a board is archived.
func (s Store) Archive(ctx context.Context, id, owner string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE projects SET archived_at = coalesce(archived_at, now()) WHERE id = $1 AND owner_clerk_id = $2`, id, owner)
		if err := exec(tag, err); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE sprints SET closed_at = now() WHERE closed_at IS NULL AND board_id IN (SELECT id FROM boards WHERE project_id = $1)`,
			`UPDATE tickets SET planned_sprint_id = NULL, estimate = NULL, updated_at = now()
				WHERE planned_sprint_id IN (SELECT ps.id FROM planned_sprints ps JOIN boards b ON b.id = ps.board_id WHERE b.project_id = $1)`,
			`DELETE FROM planned_sprints WHERE board_id IN (SELECT id FROM boards WHERE project_id = $1)`,
		} {
			if _, err := tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Restore puts an archived project back on the lists. Sprints start again by hand.
func (s Store) Restore(ctx context.Context, id, owner string) error {
	return exec(s.DB.Exec(ctx, `UPDATE projects SET archived_at = NULL WHERE id = $1 AND owner_clerk_id = $2`, id, owner))
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

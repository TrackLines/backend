package roadmaps

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

var (
	ErrNotFound          = errors.New("roadmap not found")
	ErrInvalidVisibility = errors.New("visibility must be public, login_only or team")
	ErrInvalidItemStatus = errors.New("item manual_status must be not_started, in_progress or done")
)

const (
	Public    = "public"
	LoginOnly = "login_only"
	Team      = "team"

	StatusNotStarted = "not_started"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
)

type Item struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	StartDate    *string        `json:"start_date"`  // YYYY-MM-DD or null (Gantt bar start)
	TargetDate   *string        `json:"target_date"` // YYYY-MM-DD or null
	Position     int            `json:"position"`
	ManualStatus string         `json:"manual_status"`     // used while no tickets are linked
	Status       string         `json:"status"`            // manual without links, otherwise derived from ticket progress
	Progress     Progress       `json:"progress"`          // linked tickets done / total — shown to everyone
	Tickets      []LinkedTicket `json:"tickets,omitempty"` // owner only (stripped for other viewers)
}

type Progress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// LinkedTicket is a ticket delivering a roadmap item; Done uses the board's-last-column rule.
type LinkedTicket struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

type Roadmap struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	OwnerClerkID string   `json:"-"` // never exposed: public roadmaps would leak Clerk ids
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Visibility   string   `json:"visibility"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Progress     Progress `json:"progress"`
	Items        []Item   `json:"items,omitempty"`
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
	rows, err := s.DB.Query(ctx, `SELECT id, title, description, start_date::text, target_date::text, position, manual_status
		FROM roadmap_items WHERE roadmap_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	r.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
		var i Item
		err := row.Scan(&i.ID, &i.Title, &i.Description, &i.StartDate, &i.TargetDate, &i.Position, &i.ManualStatus)
		return i, err
	})
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int, len(r.Items))
	for n, i := range r.Items {
		idx[i.ID] = n
	}
	allTickets := []LinkedTicket{}
	links, err := s.DB.Query(ctx, `SELECT l.item_id, t.id, t.title, `+boards.TicketDoneSQL+`
		FROM roadmap_item_tickets l JOIN roadmap_items ri ON ri.id = l.item_id JOIN tickets t ON t.id = l.ticket_id
		WHERE ri.roadmap_id = $1 ORDER BY t.title`, id)
	if err != nil {
		return nil, err
	}
	defer links.Close()
	for links.Next() {
		var itemID string
		var lt LinkedTicket
		if err := links.Scan(&itemID, &lt.ID, &lt.Title, &lt.Done); err != nil {
			return nil, err
		}
		it := &r.Items[idx[itemID]]
		it.Tickets = append(it.Tickets, lt)
		allTickets = append(allTickets, lt)
		it.Progress.Total++
		if lt.Done {
			it.Progress.Done++
		}
	}
	for n := range r.Items {
		r.Items[n].Status = DeriveItemStatus(r.Items[n].ManualStatus, r.Items[n].Progress)
	}
	r.Progress = AggregateProgress(allTickets)
	return &r, links.Err()
}

// DeriveItemStatus follows linked ticket completion while preserving the owner's manual status
// for items that have no linked tickets.
func DeriveItemStatus(manual string, progress Progress) string {
	if progress.Total == 0 {
		if ValidManualStatus(manual) {
			return manual
		}
		return StatusNotStarted
	}
	if progress.Done <= 0 {
		return StatusNotStarted
	}
	if progress.Done >= progress.Total {
		return StatusDone
	}
	return StatusInProgress
}

func ValidManualStatus(status string) bool {
	return status == StatusNotStarted || status == StatusInProgress || status == StatusDone
}

func AggregateProgress(tickets []LinkedTicket) Progress {
	var total Progress
	seen := make(map[string]struct{}, len(tickets))
	for _, ticket := range tickets {
		if _, ok := seen[ticket.ID]; ok {
			continue
		}
		seen[ticket.ID] = struct{}{}
		total.Total++
		if ticket.Done {
			total.Done++
		}
	}
	return total
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
	for _, i := range items {
		if i.ManualStatus != "" && !ValidManualStatus(i.ManualStatus) {
			return ErrInvalidItemStatus
		}
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := exec(tx.Exec(ctx, `UPDATE roadmaps SET updated_at = now()
			WHERE id = $1 AND owner_clerk_id = $2`, id, owner)); err != nil {
			return err
		}
		// Keep item identity (and so its linked tickets): items whose id already belongs to this
		// roadmap are updated in place, the rest are inserted, and missing ones are deleted.
		kept := []string{}
		for pos, i := range items {
			tag, err := tx.Exec(ctx, `UPDATE roadmap_items SET title = $3, description = $4, target_date = $5::date, position = $6, start_date = $7::date,
				manual_status = COALESCE(NULLIF($8, ''), manual_status)
				WHERE id::text = $1 AND roadmap_id = $2`, i.ID, id, i.Title, i.Description, i.TargetDate, pos, i.StartDate, i.ManualStatus)
			if err != nil {
				return datesErr(err)
			}
			if tag.RowsAffected() == 1 {
				kept = append(kept, i.ID)
				continue
			}
			var newID string
			if err := tx.QueryRow(ctx, `INSERT INTO roadmap_items (roadmap_id, title, description, target_date, position, start_date, manual_status)
				VALUES ($1, $2, $3, $4::date, $5, $6::date, COALESCE(NULLIF($7, ''), $8)) RETURNING id`, id, i.Title, i.Description, i.TargetDate, pos, i.StartDate, i.ManualStatus, StatusNotStarted).Scan(&newID); err != nil {
				return datesErr(err)
			}
			kept = append(kept, newID)
		}
		_, err := tx.Exec(ctx, `DELETE FROM roadmap_items WHERE roadmap_id = $1 AND NOT (id = ANY($2::uuid[]))`, id, kept)
		return err
	})
}

var (
	ErrOtherProject = errors.New("linked tickets must be in the roadmap's project")
	ErrBadDates     = errors.New("an item's start date must be on or before its target date")
)

// ValidDates checks an item's start/target (YYYY-MM-DD strings compare correctly as text).
func ValidDates(i Item) bool {
	return i.StartDate == nil || i.TargetDate == nil || *i.StartDate == "" || *i.TargetDate == "" || *i.StartDate <= *i.TargetDate
}

// SetItemTickets replaces the tickets linked to item on the owner's roadmap. Tickets must be in the
// roadmap's project.
func (s Store) SetItemTickets(ctx context.Context, owner, roadmapID, itemID string, ticketIDs []string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var projectID string
		if err := tx.QueryRow(ctx, `SELECT r.project_id FROM roadmap_items ri JOIN roadmaps r ON r.id = ri.roadmap_id
			WHERE ri.id = $1 AND r.id = $2 AND r.owner_clerk_id = $3 FOR UPDATE OF ri`, itemID, roadmapID, owner).Scan(&projectID); err != nil {
			return notFound(err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT id) FROM tickets WHERE id = ANY($1::uuid[]) AND project_id = $2`, ticketIDs, projectID).Scan(&n); err != nil {
			return notFound(err)
		}
		uniq := map[string]bool{}
		for _, t := range ticketIDs {
			uniq[t] = true
		}
		if n != len(uniq) {
			return ErrOtherProject
		}
		if _, err := tx.Exec(ctx, `DELETE FROM roadmap_item_tickets WHERE item_id = $1`, itemID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO roadmap_item_tickets (item_id, ticket_id) SELECT $1, x FROM unnest($2::uuid[]) AS x ON CONFLICT DO NOTHING`, itemID, ticketIDs)
		return err
	})
}

// datesErr turns the start<=target CHECK violation (23514) into ErrBadDates.
func datesErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23514" {
		return ErrBadDates
	}
	return err
}

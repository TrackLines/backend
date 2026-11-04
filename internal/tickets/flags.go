package tickets

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/webhooks"
)

// Feature flags in flags.gg that follow a ticket across its board (TL-784). Every call to flags.gg is
// queued in flag_outbox, in the transaction that caused it (a rollback sends nothing), and sent by the
// integrations worker in order per ticket. Nothing is queued unless the org has connected flags.gg.

var (
	ErrBadFlagName    = httpx.Code("bad_flag_name", "a flag name is 1–100 letters, digits, spaces, dots, dashes or underscores")
	ErrFlagNameLocked = httpx.Code("flag_name_locked", "the flag already exists in flags.gg: its name can't change")
)

var flagName = regexp.MustCompile(`^[\p{L}\p{N} _.\-]{1,100}$`)

// queueFlag queues action for ticket id, if org has connected flags.gg.
func queueFlag(ctx context.Context, q webhooks.Querier, org, id, action string) error {
	_, err := q.Exec(ctx, `INSERT INTO flag_outbox (org_id, ticket_id, action)
		SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM org_integrations WHERE org_id = $1 AND provider = 'flags')`, org, id, action)
	return err
}

// flagsOnMove queues what a move does to the ticket's flag: off the backlog onto a board creates it,
// then the column it arrives in may promote or archive it. Going back (or to the backlog) does nothing:
// flags.gg never demotes. A "Remove flag" ticket reaching Done archives its ticket's flag (TL-788).
func flagsOnMove(ctx context.Context, q webhooks.Querier, org, id string, from, to *string, done bool) error {
	if done {
		if err := archiveCleanedUp(ctx, q, org, id); err != nil {
			return err
		}
	}
	if to == nil {
		return nil
	}
	var needs bool
	var action string
	if err := q.QueryRow(ctx, `SELECT t.needs_flag, `+boards.FlagActionSQL("c", "b")+`
		FROM tickets_all t JOIN columns c ON c.id = $2 JOIN boards b ON b.id = c.board_id WHERE t.id = $1`, id, *to).Scan(&needs, &action); err != nil {
		return err
	}
	if !needs {
		return nil
	}
	if from == nil {
		if err := queueFlag(ctx, q, org, id, "create"); err != nil {
			return err
		}
	}
	if action == "none" {
		return nil
	}
	return queueFlag(ctx, q, org, id, action)
}

// archiveCleanedUp: id is done; if it's some ticket's "Remove flag" ticket, that flag is archived.
func archiveCleanedUp(ctx context.Context, q webhooks.Querier, org, id string) error {
	_, err := q.Exec(ctx, `INSERT INTO flag_outbox (org_id, ticket_id, action)
		SELECT $1, o.id, 'archive' FROM tickets_all o
		WHERE o.flag_cleanup_id = $2 AND EXISTS (SELECT 1 FROM org_integrations WHERE org_id = $1 AND provider = 'flags')`, org, id)
	return err
}

// flagGone: the ticket was deleted, closed as won't do or marked a duplicate, so its flag is archived.
func flagGone(ctx context.Context, q webhooks.Querier, org, id string) error {
	var needs bool
	if err := q.QueryRow(ctx, `SELECT needs_flag FROM tickets_all WHERE id = $1`, id).Scan(&needs); err != nil || !needs {
		return err
	}
	return queueFlag(ctx, q, org, id, "archive")
}

// FlagInput is what a create or update sets about a ticket's flag; nil fields stay as they are.
type FlagInput struct {
	Needs   *bool
	Name    *string // "" goes back to the key lower-cased
	AgentID *string // "" goes back to the project's default
}

// SetFlag changes whether ticket id needs a flag, its name and agent. Ticking it on a ticket already on a
// board creates the flag now. The name can't change once flags.gg has the flag. Unticking it leaves the
// flag in flags.gg: removing it is a decision (RemoveFlag).
func (s Store) SetFlag(ctx context.Context, owner, id string, in FlagInput) error {
	if in.Name != nil {
		*in.Name = strings.TrimSpace(*in.Name)
		if *in.Name != "" && !flagName.MatchString(*in.Name) {
			return ErrBadFlagName
		}
	}
	if in.AgentID != nil {
		*in.AgentID = strings.TrimSpace(*in.AgentID)
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var was, onBoard, created bool
		var name *string
		if err := tx.QueryRow(ctx, `SELECT t.needs_flag, t.column_id IS NOT NULL, t.flag_created_at IS NOT NULL, t.flag_name FROM tickets t
			JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2 WHERE t.id = $1 FOR UPDATE OF t`, id, owner).Scan(&was, &onBoard, &created, &name); err != nil {
			return notFound(err)
		}
		if in.Name != nil && created {
			current := ""
			if name != nil {
				current = *name
			}
			if current != *in.Name {
				return ErrFlagNameLocked
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets SET needs_flag = COALESCE($2, needs_flag),
				flag_name = CASE WHEN $3::text IS NULL THEN flag_name ELSE NULLIF($3, '') END,
				flag_agent_id = CASE WHEN $4::text IS NULL THEN flag_agent_id ELSE NULLIF($4, '') END,
				updated_at = now() WHERE id = $1`, id, in.Needs, in.Name, in.AgentID); err != nil {
			return err
		}
		if in.Needs != nil && *in.Needs && !was && onBoard {
			return queueFlag(ctx, tx, owner, id, "create")
		}
		return nil
	})
}

// RemoveFlag is "Remove flag": the code no longer checks it, so it's archived in flags.gg and the ticket
// stops needing one.
func (s Store) RemoveFlag(ctx context.Context, owner, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tickets t SET needs_flag = false, flag_error = NULL, updated_at = now()
			FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2`, id, owner)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return queueFlag(ctx, tx, owner, id, "archive")
	})
}

// FlagTicket is what the integrations need to act on a ticket's flag.
type FlagTicket struct {
	ID, ProjectID, Key, Title string
	Name                      string  // the flag's name in flags.gg
	AgentID                   string  // the ticket's agent, else its board's, else the project's default; "" when neither
	AssignedTo                *string // who gets its "Remove flag" ticket
	CleanupDays               int
	CleanupID                 *string
	NeedsFlag                 bool
}

// FlagTicketOf reads ticket id (deleted ones too: their flag is still archived) for org.
func (s Store) FlagTicketOf(ctx context.Context, org, id string) (*FlagTicket, error) {
	var f FlagTicket
	err := s.DB.QueryRow(ctx, `SELECT t.id::text, t.project_id::text, p.key_prefix || '-' || t.number, t.title,
			COALESCE(t.flag_name, lower(p.key_prefix || '-' || t.number)), COALESCE(t.flag_agent_id, b.flags_agent_id, p.flags_agent_id, ''),
			t.assigned_to, p.flag_cleanup_days, t.flag_cleanup_id::text, t.needs_flag
		FROM tickets_all t JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		LEFT JOIN boards b ON b.id = t.board_id WHERE t.id::text = $1`, id, org).
		Scan(&f.ID, &f.ProjectID, &f.Key, &f.Title, &f.Name, &f.AgentID, &f.AssignedTo, &f.CleanupDays, &f.CleanupID, &f.NeedsFlag)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &f, err
}

// FlagSent records what flags.gg said to a job for ticket id: created (the name is locked from now),
// archived (a new flag can be made later), ok, or failed with msg, shown on the ticket.
func (s Store) FlagSent(ctx context.Context, id, outcome, msg string) error {
	var sql string
	switch outcome {
	case "created":
		sql = `UPDATE tickets_all SET flag_created_at = COALESCE(flag_created_at, now()), flag_error = NULL WHERE id = $1`
	case "archived":
		sql = `UPDATE tickets_all SET flag_created_at = NULL, flag_error = NULL WHERE id = $1`
	case "failed":
		_, err := s.DB.Exec(ctx, `UPDATE tickets_all SET flag_error = $2 WHERE id = $1`, id, msg)
		return err
	default:
		sql = `UPDATE tickets_all SET flag_error = NULL WHERE id = $1`
	}
	_, err := s.DB.Exec(ctx, sql, id)
	return err
}

// CreateFlagCleanup makes f's "Remove flag" ticket (TL-788) once: a backlog task due due (YYYY-MM-DD),
// for whoever had f, labelled flag-cleanup. It answers the existing one's id if there is one.
func (s Store) CreateFlagCleanup(ctx context.Context, org string, f *FlagTicket, due string) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var existing *string
		if err := tx.QueryRow(ctx, `SELECT flag_cleanup_id::text FROM tickets_all WHERE id = $1 FOR UPDATE`, f.ID).Scan(&existing); err != nil {
			return err
		}
		if existing != nil {
			id = *existing
			return nil
		}
		desc := "`" + f.Name + "` is in production for " + f.Key + " (" + f.Title + ").\n\n" +
			"Once it's on for everyone and staying that way, take the checks for `" + f.Name + "` out of the code, then finish this ticket: that archives the flag in flags.gg."
		if err := tx.QueryRow(ctx, `INSERT INTO tickets (project_id, type, title, description, priority, position, created_by, assigned_to, due_date)
			SELECT p.id, 'task', $2, $3, 'medium', COALESCE((SELECT max(position) + 1 FROM tickets x WHERE x.project_id = p.id AND x.board_id IS NULL), 0), 'flags.gg', $4, $5::date
			FROM projects p WHERE p.id = $1 RETURNING id::text`, f.ProjectID, "Remove flag "+f.Name, desc, f.AssignedTo, due).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ticket_labels (ticket_id, label) VALUES ($1, 'flag-cleanup')`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets_all SET flag_cleanup_id = $2 WHERE id = $1`, f.ID, id); err != nil {
			return err
		}
		return emit(ctx, tx, org, "ticket.created", id)
	})
	return id, err
}

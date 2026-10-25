package imports

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/tickets"
)

var (
	ErrNotFound   = errors.New("import not found")
	ErrProblems   = httpx.Code("import_problems", "fix the problems in the preview before running the import")
	ErrNotPreview = httpx.Code("import_not_previewing", "only an import that's still a preview can be run")
	ErrNotRunning = httpx.Code("import_not_running", "only a preview or a running import can be cancelled")
	ErrNotDone    = httpx.Code("import_not_done", "only a finished import can be scrapped")
	ErrChanged    = httpx.Code("import_changed", "someone has worked on what this import created, so it can't be scrapped")
	errCancelled  = errors.New("cancelled")
)

// Import is one run, as the API shows it. Problems, Skipped, Planned and Projects are worked out
// from the plan against the org as it is now, while it's a preview.
type Import struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	State      string    `json:"state"` // previewing, running, cancelling, done, failed, cancelled, scrapped
	Problems   []Problem `json:"problems,omitempty"`
	Skipped    []Skip    `json:"skipped,omitempty"`
	Planned    *Counts   `json:"planned,omitempty"`
	Projects   []Preview `json:"projects,omitempty"`
	Progress   int       `json:"progress"` // tickets created, of total
	Total      int       `json:"total"`
	Report     *Report   `json:"report"`
	Error      string    `json:"error,omitempty"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  string    `json:"created_at"`
	StartedAt  *string   `json:"started_at"`
	FinishedAt *string   `json:"finished_at"`
}

// Report is what a finished run created and left out.
type Report struct {
	Created Counts `json:"created"`
	Skipped []Skip `json:"skipped"`
}

// Preview is a project the import creates, without its tickets.
type Preview struct {
	SourceID string         `json:"source_id"`
	Name     string         `json:"name"`
	Prefix   string         `json:"prefix"`
	Tickets  int            `json:"tickets"`
	Boards   []BoardPreview `json:"boards"`
}

type BoardPreview struct {
	SourceID string   `json:"source_id"`
	Name     string   `json:"name"`
	TeamID   *string  `json:"team_id"`
	Columns  []string `json:"columns"`
}

type Store struct{ DB *pgxpool.Pool }

// a running import that hasn't beaten its heart for this long died with its server: it reads as failed
const stale = `(state IN ('running', 'cancelling') AND updated_at < now() - interval '10 minutes')`

const importCols = `id::text, source, CASE WHEN ` + stale + ` THEN 'failed' ELSE state END, progress, total, report,
	CASE WHEN ` + stale + ` THEN 'interrupted' ELSE error END, created_by,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(finished_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func scanImport(row pgx.Row, extra ...any) (Import, error) {
	var im Import
	var report []byte
	err := row.Scan(append([]any{&im.ID, &im.Source, &im.State, &im.Progress, &im.Total, &report, &im.Error, &im.CreatedBy,
		&im.CreatedAt, &im.StartedAt, &im.FinishedAt}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) || isBadUUID(err) {
		return im, ErrNotFound
	}
	if err == nil && report != nil {
		err = json.Unmarshal(report, &im.Report)
	}
	return im, err
}

func isBadUUID(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "22P02"
}

// orgOf is what a plan is checked against.
func (s Store) orgOf(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, owner string) (org, error) {
	var o org
	limit, err := billing.ProjectLimit(ctx, s.DB, owner)
	if err != nil {
		return o, err
	}
	o.limit = limit
	err = q.QueryRow(ctx, `SELECT count(*), coalesce(array_agg(key_prefix), '{}') FROM projects WHERE owner_clerk_id = $1`, owner).Scan(&o.projects, &o.prefixes)
	return o, err
}

// Create stores p as a preview.
func (s Store) Create(ctx context.Context, owner, user, source string, p Plan) (*Import, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var id string
	if err := s.DB.QueryRow(ctx, `INSERT INTO imports (org_id, source, plan, created_by) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		owner, source, raw, user).Scan(&id); err != nil {
		return nil, err
	}
	return s.Get(ctx, owner, id)
}

// Get is one of the org's imports; a preview comes with its problems, skips and what it creates.
func (s Store) Get(ctx context.Context, owner, id string) (*Import, error) {
	var raw []byte
	im, err := scanImport(s.DB.QueryRow(ctx, `SELECT `+importCols+`, plan FROM imports WHERE id::text = $1 AND org_id = $2`, id, owner), &raw)
	if err != nil {
		return nil, err
	}
	if im.State != "previewing" {
		return &im, nil
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	o, err := s.orgOf(ctx, s.DB, owner)
	if err != nil {
		return nil, err
	}
	problems, skips, n := check(p, o)
	im.Problems, im.Skipped, im.Planned, im.Total = problems, skips, &n, n.Tickets
	for _, pr := range p.Projects {
		pv := Preview{SourceID: pr.SourceID, Name: pr.Name, Prefix: pr.Prefix, Tickets: len(pr.Tickets), Boards: []BoardPreview{}}
		for _, b := range pr.Boards {
			bp := BoardPreview{SourceID: b.SourceID, Name: b.Name, TeamID: b.TeamID, Columns: []string{}}
			for _, c := range b.Columns {
				bp.Columns = append(bp.Columns, c.Name)
			}
			pv.Boards = append(pv.Boards, bp)
		}
		im.Projects = append(im.Projects, pv)
	}
	return &im, nil
}

// List is the org's imports, newest first, without their plans.
func (s Store) List(ctx context.Context, owner string) ([]Import, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+importCols+` FROM imports WHERE org_id = $1 ORDER BY created_at DESC LIMIT 50`, owner)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Import, error) { return scanImport(row) })
	if out == nil {
		out = []Import{}
	}
	return out, err
}

// Start turns a preview with no problems into a running import and hands back its plan.
func (s Store) Start(ctx context.Context, owner, id string) (Plan, error) {
	var p Plan
	im, err := s.Get(ctx, owner, id)
	if err != nil {
		return p, err
	}
	if im.State != "previewing" {
		return p, ErrNotPreview
	}
	if len(im.Problems) > 0 {
		return p, ErrProblems
	}
	var raw []byte
	err = s.DB.QueryRow(ctx, `UPDATE imports SET state = 'running', started_at = now(), updated_at = now(), total = $3
		WHERE id::text = $1 AND org_id = $2 AND state = 'previewing' RETURNING plan`, id, owner, im.Total).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotPreview // started by someone else in the meantime
	}
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(raw, &p)
}

// Cancel stops a preview at once, or asks a running import to stop (it rolls back, creating nothing).
func (s Store) Cancel(ctx context.Context, owner, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE imports SET state = CASE state WHEN 'previewing' THEN 'cancelled' ELSE 'cancelling' END,
			finished_at = CASE state WHEN 'previewing' THEN now() END
		WHERE id::text = $1 AND org_id = $2 AND state IN ('previewing', 'running') AND NOT `+stale, id, owner)
	if err != nil && !isBadUUID(err) {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, owner, id); err != nil {
			return err
		}
		return ErrNotRunning
	}
	return nil
}

// Run creates everything in p in one transaction, so a failed or cancelled run leaves nothing behind.
// Progress is written outside it as it goes; cancelling is noticed between tickets.
func (s Store) Run(ctx context.Context, owner, user, id, source string, p Plan) {
	report := Report{Skipped: []Skip{}}
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		return s.build(ctx, tx, owner, user, id, source, p, &report)
	})
	state, msg := "done", ""
	switch {
	case errors.Is(err, errCancelled):
		state = "cancelled"
	case err != nil:
		state, msg = "failed", err.Error()
	}
	raw, _ := json.Marshal(report)
	if state != "done" {
		raw = nil
	}
	_, _ = s.DB.Exec(context.WithoutCancel(ctx), `UPDATE imports SET state = $2, error = $3, report = $4, finished_at = now(), updated_at = now()
		WHERE id::text = $1`, id, state, msg, raw)
}

// labelOK: ticket_labels takes 1–50 characters.
func labelOK(l string) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(l))
	return n > 0 && n <= 50
}

func (s Store) build(ctx context.Context, tx pgx.Tx, owner, user, id, source string, p Plan, report *Report) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('projects:' || $1))`, owner); err != nil { // as projects.Create
		return err
	}
	o, err := s.orgOf(ctx, tx, owner)
	if err != nil {
		return err
	}
	problems, skips, _ := check(p, o)
	if len(problems) > 0 { // the org changed since the preview (a project was made, the plan downgraded)
		return fmt.Errorf("%s", problems[0].Message)
	}
	report.Skipped = append(report.Skipped, skips...)
	item := func(kind, sourceID, target string) error {
		_, err := tx.Exec(ctx, `INSERT INTO import_items (import_id, kind, source_id, target_id) VALUES ($1, $2, $3, $4)`, id, kind, sourceID, target)
		return err
	}
	done, now := 0, time.Now()
	for _, pr := range p.Projects {
		var projectID string
		if err := tx.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, description, key_prefix) VALUES ($1, $2, $3, $4) RETURNING id::text`,
			owner, pr.Name, pr.Description, pr.Prefix).Scan(&projectID); err != nil {
			return err
		}
		if err := item("project", pr.SourceID, projectID); err != nil {
			return err
		}
		report.Created.Projects++
		columns := map[string]string{} // board/column source ids → column id
		lastOf := map[string]string{}  // board source id → its Done column's id
		boardOf := map[string]string{} // board source id → board id
		scales := map[string]string{}
		for _, b := range pr.Boards {
			t := boards.Template{Style: "sprints", EstimateScale: scale(b)}
			for _, c := range b.Columns {
				t.Columns = append(t.Columns, boards.TemplateColumn{Name: c.Name})
			}
			nb, err := boards.CreateFromTx(ctx, tx, owner, projectID, b.Name, "", t)
			if err != nil {
				return err
			}
			if b.TeamID != nil { // the team must be the org's (project_teams' foreign key checks)
				if _, err := tx.Exec(ctx, `INSERT INTO project_teams (project_id, team_id, org_id) VALUES ($1, $2::uuid, $3) ON CONFLICT DO NOTHING`,
					projectID, *b.TeamID, owner); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE boards SET team_id = $2::uuid WHERE id = $1`, nb.ID, *b.TeamID); err != nil {
					return err
				}
			}
			boardOf[b.SourceID], scales[b.SourceID], lastOf[b.SourceID] = nb.ID, t.EstimateScale, nb.Columns[len(nb.Columns)-1].ID
			if err := item("board", b.SourceID, nb.ID); err != nil {
				return err
			}
			report.Created.Boards++
			for i, c := range b.Columns {
				columns[b.SourceID+"/"+c.SourceID] = nb.Columns[i].ID
				if err := item("column", b.SourceID+"/"+c.SourceID, nb.Columns[i].ID); err != nil {
					return err
				}
				report.Created.Columns++
			}
		}
		ticketOf := map[string]string{}
		positions := map[string]int{} // per column ("" for the backlog)
		known := map[string]bool{}
		for k := range columns {
			known[k] = true
		}
		for _, t := range pr.Tickets {
			if placeable(t, known) != "" {
				continue // reported as skipped by check
			}
			var board, column *string
			if t.Board != "" && t.Type != tickets.TypeEpic { // epics live in the backlog (TL-213)
				b, c := boardOf[t.Board], columns[t.Board+"/"+t.Column]
				board, column = &b, &c
			}
			key := ""
			if column != nil {
				key = *column
			}
			estimate := &t.Estimate
			if t.Estimate == "" || column == nil || !boards.ValidEstimate(scales[t.Board], t.Estimate) {
				if t.Estimate != "" {
					reason := fmt.Sprintf("%q isn't on the board's scale", t.Estimate)
					if column == nil {
						reason = "backlog tickets aren't estimated"
					}
					report.Skipped = append(report.Skipped, Skip{Kind: "estimate", SourceID: t.SourceID, Reason: reason})
				}
				estimate = nil
			}
			priority := t.Priority
			if priority == "" {
				priority = "medium"
			}
			// done: in its board's Done column (reports need when), or finished and in the backlog (resolved)
			var doneAt *time.Time
			resolved := column == nil && t.Done
			if resolved || (column != nil && *column == lastOf[t.Board]) {
				doneAt = cmp.Or(t.DoneAt, t.UpdatedAt, t.CreatedAt, &now)
			}
			var ticketID string
			if err := tx.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, type, title, description, priority, position, created_by,
					estimate, number, assigned_to, external_source, external_project, external_id, external_url, created_at, updated_at, due_date,
					done_at, resolved_at, resolution)
				VALUES ($1, $2, $3, $4::ticket_type, $5, $6, $7::priority, $8, $9, $10, $11, $12, $13, $14, $15, nullif($16, ''),
					coalesce($17, now()), coalesce($18, $17, now()), $19::date,
					$20, CASE WHEN $21 THEN $20::timestamptz END, CASE WHEN $21 THEN 'done' END) RETURNING id::text`,
				projectID, board, column, t.Type, t.Title, t.Description, priority, positions[key], user,
				estimate, t.Number, t.Assignee, source, pr.SourceID, t.SourceID, t.URL, t.CreatedAt, t.UpdatedAt, t.DueDate,
				doneAt, resolved).Scan(&ticketID); err != nil {
				return fmt.Errorf("%s-%d: %w", pr.Prefix, t.Number, err)
			}
			positions[key]++
			ticketOf[t.SourceID] = ticketID
			if err := item("ticket", t.SourceID, ticketID); err != nil {
				return err
			}
			report.Created.Tickets++
			var labels []string
			for _, l := range t.Labels {
				if labelOK(l) {
					labels = append(labels, strings.TrimSpace(l))
				}
			}
			if len(labels) > 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO ticket_labels (ticket_id, label) SELECT $1, l FROM unnest($2::text[]) l ON CONFLICT DO NOTHING`, ticketID, labels); err != nil {
					return err
				}
			}
			for _, c := range t.Comments {
				if strings.TrimSpace(c.Body) == "" {
					report.Skipped = append(report.Skipped, Skip{Kind: "comment", SourceID: c.SourceID, Reason: "empty"})
					continue
				}
				var commentID string
				if err := tx.QueryRow(ctx, `INSERT INTO ticket_comments (ticket_id, body, author, created_at) VALUES ($1, $2, $3, coalesce($4, now())) RETURNING id::text`,
					ticketID, c.Body, c.Author, c.CreatedAt).Scan(&commentID); err != nil {
					return err
				}
				if err := item("comment", c.SourceID, commentID); err != nil {
					return err
				}
				report.Created.Comments++
			}
			if done++; done%50 == 0 {
				if err := s.beat(ctx, id, done); err != nil {
					return err
				}
			}
		}
		for _, t := range pr.Tickets { // parents once every ticket exists
			if child, parent := ticketOf[t.SourceID], ticketOf[t.Parent]; child != "" && parent != "" {
				if _, err := tx.Exec(ctx, `UPDATE tickets SET parent_id = $2 WHERE id = $1`, child, parent); err != nil {
					return err
				}
			} else if child != "" && t.Parent != "" {
				report.Skipped = append(report.Skipped, Skip{Kind: "parent", SourceID: t.SourceID, Reason: "its parent isn't in the import"})
			}
			// and what it waits on (TL-538); ponytail: a two-way block keeps one side, longer cycles aren't looked for
			for _, b := range t.BlockedBy {
				if child, blocker := ticketOf[t.SourceID], ticketOf[b]; child == "" {
					break
				} else if blocker == "" || blocker == child {
					report.Skipped = append(report.Skipped, Skip{Kind: "blocked_by", SourceID: t.SourceID, Reason: fmt.Sprintf("what blocks it (%s) isn't in the project's import", b)})
				} else if _, err := tx.Exec(ctx, `INSERT INTO ticket_dependencies (ticket_id, blocked_by_id) SELECT $1, $2
					WHERE NOT EXISTS (SELECT 1 FROM ticket_dependencies WHERE ticket_id = $2 AND blocked_by_id = $1) ON CONFLICT DO NOTHING`, child, blocker); err != nil {
					return err
				}
			}
		}
	}
	if err := s.beat(ctx, id, done); err != nil {
		return err
	}
	// what each ticket looks like now: scrapping compares against it
	_, err = tx.Exec(ctx, `UPDATE import_items i SET fingerprint = (SELECT `+fingerprint+` FROM tickets_all t WHERE t.id = i.target_id)
		WHERE i.import_id = $1 AND i.kind = 'ticket'`, id)
	return err
}

// beat records progress (outside the run's transaction, so it shows while it runs) and notices a cancel.
func (s Store) beat(ctx context.Context, id string, done int) error {
	var state string
	if err := s.DB.QueryRow(ctx, `UPDATE imports SET progress = $2, updated_at = now() WHERE id::text = $1 RETURNING state`, id, done).Scan(&state); err != nil {
		return err
	}
	if state == "cancelling" {
		return errCancelled
	}
	return nil
}

// fingerprint is a ticket (alias t) with its labels, comments, attachments and history: anything
// done to it changes it. ponytail: the whole row, so a migration adding a ticket column also reads
// as a change and blocks scrapping older imports; list the columns if that ever matters.
const fingerprint = `md5(row_to_json(t)::text
	|| coalesce((SELECT string_agg(lower(l.label), ',' ORDER BY lower(l.label)) FROM ticket_labels l WHERE l.ticket_id = t.id), '')
	|| coalesce((SELECT string_agg(c.id::text || md5(c.body), ',' ORDER BY c.id) FROM ticket_comments c WHERE c.ticket_id = t.id), '')
	|| (SELECT count(*) FROM ticket_attachments a WHERE a.ticket_id = t.id)
	|| '/' || (SELECT count(*) FROM ticket_history h WHERE h.ticket_id = t.id))`

// Scrap deletes everything a finished import created, as long as nobody has acted on it: every
// imported ticket is as it was imported, and nobody has added tickets to its projects.
func (s Store) Scrap(ctx context.Context, owner, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM imports WHERE id::text = $1 AND org_id = $2 FOR UPDATE`, id, owner).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isBadUUID(err) {
				return ErrNotFound
			}
			return err
		}
		if state != "done" {
			return ErrNotDone
		}
		var changed bool
		if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM import_items i LEFT JOIN tickets_all t ON t.id = i.target_id
				WHERE i.import_id = $1::uuid AND i.kind = 'ticket' AND (t.id IS NULL OR i.fingerprint IS DISTINCT FROM `+fingerprint+`))
			OR EXISTS (SELECT 1 FROM tickets_all t JOIN import_items p ON p.target_id = t.project_id AND p.kind = 'project' AND p.import_id = $1::uuid
				WHERE NOT EXISTS (SELECT 1 FROM import_items i WHERE i.import_id = $1::uuid AND i.kind = 'ticket' AND i.target_id = t.id))`,
			id).Scan(&changed); err != nil {
			return err
		}
		if changed {
			return ErrChanged
		}
		// ponytail: copied attachments' files stay in UploadThing; delete them here once imports copy any (TL-540)
		if _, err := tx.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = $2
			AND id IN (SELECT target_id FROM import_items WHERE import_id = $1::uuid AND kind = 'project')`, id, owner); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE imports SET state = 'scrapped', updated_at = now() WHERE id = $1::uuid`, id)
		return err
	})
}

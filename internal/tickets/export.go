package tickets

import (
	"encoding/csv"
	"net/http"
	"regexp"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
)

// ExportHeader is the export's columns; the CSV import guesses its mapping from the same names.
var ExportHeader = []string{"Key", "Title", "Description", "Type", "Priority", "Estimate", "Labels", "Board", "Column", "State", "Assignee", "Parent", "Created", "Done", "Due"}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// CSVCell keeps a spreadsheet from running a cell as a formula (CSV injection): text starting with
// = + - @ or a tab/return gets a leading '. The CSV import takes it off again.
func CSVCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// Export: GET /api/projects/{id}/export.csv: every ticket in the project (backlog and boards, open
// and closed), oldest first, as CSV (TL-637). Labels are joined with "; ", people by name where known.
func (h System) Export(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := auth.OrgID(ctx)
	var name string
	if err := h.store.DB.QueryRow(ctx, `SELECT name FROM projects WHERE id::text = $1 AND owner_clerk_id = $2`, r.PathValue("id"), org).Scan(&name); err != nil {
		writeErr(w, r, notFound(err))
		return
	}
	rows, err := h.store.DB.Query(ctx, `SELECT p.key_prefix || '-' || t.number, t.title, t.description, t.type::text, t.priority::text,
			coalesce(t.estimate, ''), array_to_string(ARRAY(SELECT l.label FROM ticket_labels l WHERE l.ticket_id = t.id ORDER BY lower(l.label)), '; '),
			coalesce(b.name, ''), coalesce(c.name, ''),
			CASE WHEN t.duplicate_of IS NOT NULL THEN 'duplicate' WHEN t.resolution = 'wont_do' THEN 'wont_do'
				WHEN `+boards.TicketDoneSQL+` THEN 'done' ELSE 'open' END,
			coalesce(t.assigned_to, ''),
			coalesce((SELECT pp.key_prefix || '-' || pt.number FROM tickets pt JOIN projects pp ON pp.id = pt.project_id WHERE pt.id = t.parent_id), ''),
			to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			coalesce(to_char(t.done_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			coalesce(to_char(t.due_date, 'YYYY-MM-DD'), '')
		FROM tickets t JOIN projects p ON p.id = t.project_id
		LEFT JOIN boards b ON b.id = t.board_id LEFT JOIN columns c ON c.id = t.column_id
		WHERE t.project_id::text = $1 AND p.owner_clerk_id = $2 ORDER BY t.number`, r.PathValue("id"), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	defer rows.Close()
	names := map[string]string{}
	if people, err := h.People(ctx, org); err == nil {
		for _, p := range people {
			if len(p.Names) > 0 {
				names[p.ID] = p.Names[0]
			}
		}
	}
	file := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if file == "" {
		file = "project"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`-tickets.csv"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	out := csv.NewWriter(w) // streamed: a big project never sits in memory whole
	_ = out.Write(ExportHeader)
	row := make([]string, len(ExportHeader))
	for rows.Next() {
		ptrs := make([]any, len(row))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			logs.Errorf("tickets: export: %v", err)
			break
		}
		if n, ok := names[row[10]]; ok {
			row[10] = n
		}
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = CSVCell(v)
		}
		_ = out.Write(cells)
	}
	out.Flush()
}

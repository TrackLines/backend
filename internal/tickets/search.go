package tickets

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

// SearchResult is a ticket found by Search, with where it lives and a bit of matching text.
type SearchResult struct {
	Ticket
	ProjectName string  `json:"project_name"`
	BoardID     *string `json:"board_id"` // null in the backlog
	BoardName   *string `json:"board_name"`
	ColumnName  *string `json:"column_name"`
	Done        bool    `json:"done"`
	Snippet     string  `json:"snippet"` // where the words matched in the description or a comment; "" if only the title did
}

// SearchQuery is what to look for: Text matches titles, descriptions and comments (every word, as
// a prefix, so it works while typing); a ticket key (TL-123) in Text is found exactly, first.
// The rest narrow it down; empty means any.
type SearchQuery struct {
	Text       string
	ProjectID  string
	BoardID    string
	Types      []string
	Priorities []string
	Labels     []string
	Assignee   string // a user id / agent name; see Unassigned
	Unassigned bool
	Status     string // open | done | ""
	Limit      int
	Page       int // 1-based; with Limit, for "load more" (stable: rank, then updated, then id)
}

const maxSearchResults = 50

var searchWord = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// tsQuery turns what someone typed into a safe prefix query: every word must match, as a prefix
// ("crash sav" → crash:* & sav:*). Punctuation can't break the query: only words survive.
func tsQuery(text string) string {
	words := searchWord.FindAllString(strings.ToLower(text), 10)
	for i, w := range words {
		words[i] = w + ":*"
	}
	return strings.Join(words, " & ")
}

// Search finds the owner's tickets: an exact key first, then by relevance (title over description
// over comments), then most recently changed. With no text it lists by recent change, filtered.
func (s Store) Search(ctx context.Context, owner string, q SearchQuery) ([]SearchResult, error) {
	if q.Limit <= 0 || q.Limit > maxSearchResults {
		q.Limit = 20
	}
	keyID := ""
	if k := strings.TrimSpace(q.Text); IsKey(k) {
		if id, err := s.IDByKey(ctx, owner, k); err == nil {
			keyID = id
		}
	}
	// a bare number is a ticket's number without its prefix ("139" → TL-139, in any project);
	// those come first, ahead of tickets that merely mention it (TL-140)
	number, _ := strconv.Atoi(strings.TrimSpace(q.Text))
	if number < 0 {
		number = 0
	}
	query := tsQuery(q.Text)
	if keyID != "" {
		query = "" // the key is the search
	}
	rows, err := s.DB.Query(ctx, `WITH q AS (SELECT CASE WHEN $2 = '' THEN NULL ELSE to_tsquery('simple', $2) END AS query)
		SELECT `+boards.TicketCols+`, p.name, t.board_id, b.name, col.name, `+boards.TicketDoneSQL+`,
			CASE WHEN q.query IS NULL THEN '' ELSE coalesce(
				(SELECT ts_headline('simple', t.description, q.query, 'StartSel="",StopSel="",MaxWords=18,MinWords=6,MaxFragments=1')
					WHERE t.description <> '' AND to_tsvector('simple', t.description) @@ q.query),
				(SELECT ts_headline('simple', c.body, q.query, 'StartSel="",StopSel="",MaxWords=18,MinWords=6,MaxFragments=1')
					FROM ticket_comments c WHERE c.ticket_id = t.id AND c.search @@ q.query ORDER BY c.created_at DESC LIMIT 1),
				'') END
		FROM tickets t
		CROSS JOIN q
		JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $1
		LEFT JOIN boards b ON b.id = t.board_id
		LEFT JOIN columns col ON col.id = t.column_id
		WHERE ($3 = '' OR t.id::text = $3)
			AND ($3 <> '' OR q.query IS NULL OR t.search @@ q.query OR t.number = $14
				OR EXISTS (SELECT 1 FROM ticket_comments c WHERE c.ticket_id = t.id AND c.search @@ q.query))
			AND ($4 = '' OR t.project_id::text = $4)
			AND ($5 = '' OR t.board_id::text = $5)
			AND (cardinality($6::text[]) = 0 OR t.type::text = ANY($6))
			AND (cardinality($7::text[]) = 0 OR t.priority::text = ANY($7))
			AND (cardinality($8::text[]) = 0 OR EXISTS (SELECT 1 FROM ticket_labels l WHERE l.ticket_id = t.id AND lower(l.label) = ANY($8)))
			AND ($9 = '' OR t.assigned_to = $9)
			AND ($10 = false OR t.assigned_to IS NULL)
			AND ($11 = '' OR ($11 = 'done') = `+boards.TicketDoneSQL+`)
		ORDER BY t.number = $14 DESC NULLS LAST, CASE WHEN q.query IS NULL THEN 0 ELSE ts_rank(t.search, q.query) END DESC, t.updated_at DESC, t.id
		LIMIT $12 OFFSET $13`,
		owner, query, keyID, q.ProjectID, q.BoardID, nonNil(q.Types), nonNil(q.Priorities), lower(q.Labels),
		q.Assignee, q.Unassigned, q.Status, q.Limit, (max(q.Page, 1)-1)*q.Limit, number)
	if err != nil {
		return nil, notFound(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SearchResult, error) {
		var r SearchResult
		return r, row.Scan(append(boards.TicketDest(&r.Ticket), &r.ProjectName, &r.BoardID, &r.BoardName, &r.ColumnName, &r.Done, &r.Snippet)...)
	})
	if out == nil {
		out = []SearchResult{}
	}
	return out, err
}

var ErrBadPriority = errors.New("priority must be low, medium, high or urgent")

// ValidPriorities: every entry is a priority.
func ValidPriorities(ps []string) bool {
	for _, p := range ps {
		if p != "low" && p != "medium" && p != "high" && p != "urgent" {
			return false
		}
	}
	return true
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func lower(v []string) []string {
	out := make([]string, 0, len(v))
	for _, s := range v {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}

// Search: GET /api/search?q=&project=&board=&type=&priority=&label=&assignee=me|unassigned|<id>&status=open|done&limit=&page=
// "My work" is assignee=me&status=open (newest change first) across every board and project.
// type, priority and label repeat (any of).
func (h System) Search(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := SearchQuery{Text: v.Get("q"), ProjectID: v.Get("project"), BoardID: v.Get("board"),
		Types: v["type"], Priorities: v["priority"], Labels: v["label"], Status: v.Get("status")}
	for _, t := range q.Types {
		if !ValidType(t) {
			http.Error(w, "type must be bug, feature or task", http.StatusBadRequest)
			return
		}
	}
	if !ValidPriorities(q.Priorities) {
		http.Error(w, ErrBadPriority.Error(), http.StatusBadRequest)
		return
	}
	if q.Status != "" && q.Status != "open" && q.Status != "done" {
		http.Error(w, "status must be open or done", http.StatusBadRequest)
		return
	}
	switch a := v.Get("assignee"); a {
	case "":
	case "unassigned":
		q.Unassigned = true
	case "me":
		q.Assignee = auth.ActorID(r.Context())
	default:
		q.Assignee = a
	}
	if p := v.Get("page"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 1000 {
			http.Error(w, "page must be 1 or more", http.StatusBadRequest)
			return
		}
		q.Page = n
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxSearchResults {
			http.Error(w, "limit must be 1–50", http.StatusBadRequest)
			return
		}
		q.Limit = n
	}
	out, err := h.store.Search(r.Context(), auth.OrgID(r.Context()), q)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

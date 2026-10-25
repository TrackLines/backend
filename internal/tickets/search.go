package tickets

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	Creator    string // a user id / agent name of who raised it; empty = any
	Status     string // open | done | ""
	Limit      int
	Page       int    // 1-based; with Limit, for "load more" (stable: rank, then updated, then id)
	AnyWord    bool   // any word may match, most matches first (similar tickets: one typo mustn't hide them, TL-275)
	Epic       string // an epic's sub-tickets at any depth (TL-215)
	Due        string // overdue | soon (due today or tomorrow) | none (no due date) | "" (TL-631); open tickets for overdue and soon
	Today      string // the caller's local day (YYYY-MM-DD), for Due; defaults to UTC's
	SortDue    bool   // soonest due first, undated last
}

var ErrBadDue = httpx.Code("bad_due", "due must be overdue, soon or none")

const maxSearchResults = 50

var searchWord = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// tsQuery turns what someone typed into a safe prefix query: every word must match, as a prefix
// ("crash sav" → crash:* & sav:*). Punctuation can't break the query: only words survive.
// With any, a ticket matching one word will do (words under three letters are left out, or "a" would
// match everything); the rank puts those matching more words first.
func tsQuery(text string, any bool) string {
	words := searchWord.FindAllString(strings.ToLower(text), 10)
	kept := words[:0]
	for _, w := range words {
		if any && len([]rune(w)) < 3 {
			continue
		}
		kept = append(kept, w+":*")
	}
	if any {
		return strings.Join(kept, " | ")
	}
	return strings.Join(kept, " & ")
}

// Search finds the owner's tickets: an exact key first, then by relevance (title over description
// over comments), then most recently changed. With no text it lists by recent change, filtered.
func (s Store) Search(ctx context.Context, owner string, q SearchQuery) ([]SearchResult, error) {
	out, _, err := s.SearchTotal(ctx, owner, q)
	return out, err
}

// SearchTotal is Search plus how many tickets match in all, across every page (0 past the last page).
func (s Store) SearchTotal(ctx context.Context, owner string, q SearchQuery) ([]SearchResult, int, error) {
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
	if q.Today == "" {
		q.Today = time.Now().UTC().Format(time.DateOnly)
	}
	query := tsQuery(q.Text, q.AnyWord)
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
				'') END,
			count(*) OVER ()
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
			AND ($16 = '' OR t.created_by = $16)
			AND `+inEpic("$15::text")+`
			AND ($17 = '' OR ($17 = 'none' AND t.due_date IS NULL)
				OR ($17 = 'overdue' AND t.due_date < $18::date AND NOT `+boards.TicketDoneSQL+`)
				OR ($17 = 'soon' AND t.due_date BETWEEN $18::date AND $18::date + 1 AND NOT `+boards.TicketDoneSQL+`))
		ORDER BY CASE WHEN $19 THEN t.due_date END ASC NULLS LAST, t.number = $14 DESC NULLS LAST, CASE WHEN q.query IS NULL THEN 0 ELSE ts_rank(t.search, q.query) END DESC, t.updated_at DESC, t.id
		LIMIT $12 OFFSET $13`,
		owner, query, keyID, q.ProjectID, q.BoardID, nonNil(q.Types), nonNil(q.Priorities), lower(q.Labels),
		q.Assignee, q.Unassigned, q.Status, q.Limit, (max(q.Page, 1)-1)*q.Limit, number, q.Epic, q.Creator, q.Due, q.Today, q.SortDue)
	if err != nil {
		return nil, 0, notFound(err)
	}
	total := 0
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SearchResult, error) {
		var r SearchResult
		return r, row.Scan(append(boards.TicketDest(&r.Ticket), &r.ProjectName, &r.BoardID, &r.BoardName, &r.ColumnName, &r.Done, &r.Snippet, &total)...)
	})
	if out == nil {
		out = []SearchResult{}
	}
	return out, total, err
}

var ErrBadPriority = httpx.Code("bad_priority", "priority must be low, medium, high or urgent")

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

// Search: GET /api/search?q=&project=&board=&type=&priority=&label=&assignee=me|unassigned|<id>&creator=me|<id>&status=open|done&limit=&page=&total=1
// total=1 answers {"tickets": [...], "total": n} instead of the bare list.
// "My work" is assignee=me&status=open (newest change first) across every board and project.
// type, priority and label repeat (any of).
func (h System) Search(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := SearchQuery{Text: v.Get("q"), ProjectID: v.Get("project"), BoardID: v.Get("board"),
		Types: v["type"], Priorities: v["priority"], Labels: v["label"], Status: v.Get("status"), AnyWord: v.Get("match") == "any", Epic: v.Get("epic")}
	for _, t := range q.Types {
		if !ValidType(t) {
			httpx.Error(w, r, http.StatusBadRequest, ErrInvalidType)
			return
		}
	}
	if !ValidPriorities(q.Priorities) {
		httpx.Error(w, r, http.StatusBadRequest, ErrBadPriority)
		return
	}
	if q.Status != "" && q.Status != "open" && q.Status != "done" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_search_status", "status must be open or done"))
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
	if c := v.Get("creator"); c == "me" {
		q.Creator = auth.ActorID(r.Context())
	} else {
		q.Creator = c
	}
	switch q.Due = v.Get("due"); q.Due {
	case "", "overdue", "soon", "none":
	default:
		httpx.Error(w, r, http.StatusBadRequest, ErrBadDue)
		return
	}
	q.Today = time.Now().In(httpx.Zone(r.Context())).Format(time.DateOnly) // overdue is by the caller's own day
	switch v.Get("sort") {
	case "":
	case "due":
		q.SortDue = true
	default:
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_search_sort", "sort must be due, or left out"))
		return
	}
	if p := v.Get("page"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 1000 {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_page", "page must be 1 or more"))
			return
		}
		q.Page = n
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxSearchResults {
			httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_search_limit", "limit must be 1–50"))
			return
		}
		q.Limit = n
	}
	out, total, err := h.store.SearchTotal(r.Context(), auth.OrgID(r.Context()), q)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if v.Get("total") == "1" { // "6 of 9" lists: the page and how many match in all
		httpx.JSON(w, http.StatusOK, map[string]any{"tickets": out, "total": total})
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

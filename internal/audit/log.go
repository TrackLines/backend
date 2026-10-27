package audit

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/tickets"
)

var (
	ErrAuditAdmins      = httpx.Code("admins_audit_log", "only org admins, signed in, can see the audit log")
	ErrAuditPlan        = httpx.Code("audit_plan", "the audit log needs the Pro or Enterprise plan")
	ErrAuditUnavailable = httpx.Code("audit_unavailable", "the audit log can't be loaded right now: try again shortly")
)

// LogEvent is one entry in the org's audit log, as the auditor holds it.
type LogEvent struct {
	ID         string          `json:"id"`
	At         string          `json:"at"`
	Actor      *string         `json:"actor"`
	ActorKind  string          `json:"actor_kind"`
	Action     string          `json:"action"`
	TargetType *string         `json:"target_type"`
	TargetID   *string         `json:"target_id"`
	ProjectID  *string         `json:"project_id"`
	Data       json.RawMessage `json:"data"`
}

// LogPage is one page of the log, newest first.
type LogPage struct {
	Events     []LogEvent `json:"events"`
	NextCursor *string    `json:"next_cursor"`
}

// FilterError is the auditor turning a filter down (a bad time, cursor or limit), with its reason.
type FilterError string

func (e FilterError) Error() string { return string(e) }

// Log is one page of org's audit log, filtered by q (the auditor's own parameters).
func (c Client) Log(ctx context.Context, org string, q url.Values) (LogPage, error) {
	var out LogPage
	return out, c.get(ctx, "/internal/orgs/"+url.PathEscape(org)+"/events?"+q.Encode(), &out)
}

// OrgLog serves the org's audit log (TL-670): admins, signed in (never an API key), on Pro (the last year,
// as the privacy policy keeps it) or Enterprise (everything).
type OrgLog struct {
	DB      *pgxpool.Pool
	Read    func(ctx context.Context, org string, q url.Values) (LogPage, error)
	On      func() bool // the "audit-service" flag: off, the log isn't readable yet
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
	Now     func() time.Time
}

// query checks who's asking and builds the auditor's query from theirs; false: the answer's been sent.
func (h OrgLog) query(w http.ResponseWriter, r *http.Request) (string, url.Values, bool) {
	ctx := r.Context()
	user, signedIn := auth.UserID(ctx)
	org := auth.OrgID(ctx)
	admin := false
	if signedIn && org != "" && !auth.ViaAPIKey(ctx) {
		admin, _ = h.IsAdmin(ctx, org, user)
	}
	if !admin {
		httpx.Error(w, r, http.StatusForbidden, ErrAuditAdmins)
		return "", nil, false
	}
	plan, err := billing.PlanOf(ctx, h.DB, org)
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return "", nil, false
	}
	if plan == billing.Free {
		httpx.Error(w, r, http.StatusForbidden, ErrAuditPlan)
		return "", nil, false
	}
	if h.On != nil && !h.On() {
		httpx.Error(w, r, http.StatusServiceUnavailable, ErrAuditUnavailable)
		return "", nil, false
	}
	in, q := r.URL.Query(), url.Values{}
	for ours, theirs := range map[string]string{"actor": "actor", "action": "action", "target_type": "target_type", "target": "target_id",
		"project": "project_id", "from": "from", "to": "to", "cursor": "cursor", "limit": "limit"} {
		if v := in.Get(ours); v != "" {
			q.Set(theirs, v)
		}
	}
	if plan != billing.Enterprise { // Pro keeps a year: older entries never show, aged out yet or not
		now := time.Now
		if h.Now != nil {
			now = h.Now
		}
		q.Set("since", now().UTC().AddDate(-1, 0, 0).Format(time.RFC3339))
	}
	return org, q, true
}

// fail answers a read that didn't work: the auditor's reason for a bad filter, otherwise unavailable.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	if fe, bad := errors.AsType[FilterError](err); bad {
		http.Error(w, string(fe), http.StatusBadRequest)
		return
	}
	logs.Errorf("audit log: %v", err)
	httpx.Error(w, r, http.StatusServiceUnavailable, ErrAuditUnavailable)
}

// List: GET /api/org/audit?actor=&action=&target_type=&target=&project=&from=&to=&cursor=&limit= → {events, next_cursor}.
func (h OrgLog) List(w http.ResponseWriter, r *http.Request) {
	org, q, ok := h.query(w, r)
	if !ok {
		return
	}
	page, err := h.Read(r.Context(), org, q)
	if err != nil {
		fail(w, r, err)
		return
	}
	if page.Events == nil {
		page.Events = []LogEvent{}
	}
	httpx.JSON(w, http.StatusOK, page)
}

// Export: GET /api/org/audit/export.csv with the same filters (no cursor or limit): every matching entry,
// newest first, streamed a page at a time. Recorded in the log itself.
func (h OrgLog) Export(w http.ResponseWriter, r *http.Request) {
	org, q, ok := h.query(w, r)
	if !ok {
		return
	}
	q.Del("cursor")
	q.Set("limit", "200")
	page, err := h.Read(r.Context(), org, q) // the first page before any of the file: a failure is still an error
	if err != nil {
		fail(w, r, err)
		return
	}
	if _, err := h.DB.Exec(r.Context(), `SELECT audit_org($1, 'export.downloaded', 'org', $1, NULL, '{"kind":"audit_csv"}')`, org); err != nil {
		logs.Errorf("audit log: export: %v", err)
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-log.csv"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeCSV(w, page, func(cursor string) (LogPage, error) {
		q.Set("cursor", cursor)
		return h.Read(r.Context(), org, q)
	})
}

// csvHeader is the export's columns: data is the entry's details as JSON.
var csvHeader = []string{"At", "Actor", "Actor kind", "Action", "Target type", "Target", "Project", "Data"}

// writeCSV writes page and every page after it. A page failing midway ends the file there, logged: the
// status has gone already.
func writeCSV(w io.Writer, page LogPage, next func(cursor string) (LogPage, error)) {
	out := csv.NewWriter(w)
	_ = out.Write(csvHeader)
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	for {
		for _, e := range page.Events {
			_ = out.Write([]string{e.At, tickets.CSVCell(str(e.Actor)), e.ActorKind, e.Action, str(e.TargetType), tickets.CSVCell(str(e.TargetID)), str(e.ProjectID), tickets.CSVCell(string(e.Data))})
		}
		if page.NextCursor == nil {
			break
		}
		var err error
		if page, err = next(*page.NextCursor); err != nil {
			logs.Errorf("audit log: export: %v", err)
			break
		}
	}
	out.Flush()
}

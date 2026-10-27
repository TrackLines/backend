package tickets

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"

	"github.com/bugfixes/go-bugfixes/logs"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// MaxBulk is how many tickets one bulk edit can change (TL-633).
const MaxBulk = 200

var (
	ErrBulkTickets = httpx.Code("bulk_tickets", "send 1 to 200 ticket ids")
	ErrBulkAction  = httpx.Code("bulk_action", "action must be priority, labels, assign, parent, move, resolve or delete")
)

// BulkResult is what happened to one ticket: ok, or why not (the code and message a single change gives).
type BulkResult struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
}

type bulkInput struct {
	TicketIDs  []string        `json:"ticket_ids"`
	Action     string          `json:"action"`
	Priority   string          `json:"priority"`   // priority
	Add        []string        `json:"add"`        // labels
	Remove     []string        `json:"remove"`     // labels
	Assignee   json.RawMessage `json:"assignee"`   // assign: an id, or null to unassign
	ParentID   json.RawMessage `json:"parent_id"`  // parent: an id, or null to detach
	ColumnID   string          `json:"column_id"`  // move
	Estimate   *string         `json:"estimate"`   // move: entering a board that estimates
	Resolution string          `json:"resolution"` // resolve: done | wont_do
	Reason     string          `json:"reason"`     // resolve
}

// Bulk: POST /api/tickets/bulk {ticket_ids, action, …} changes many tickets at once (TL-633). Each one
// goes through the very handler a single change uses, in this request's context (same person, key,
// org and zone), so locked sprints, estimate scales, agent review, history, notifications and
// webhooks behave exactly as one at a time. One failing doesn't stop the rest; the answer says
// which, with the same error code a single change gives.
// ponytail: one ticket after another (≤ 200); run them in parallel if big bulk edits feel slow.
func (h System) Bulk(w http.ResponseWriter, r *http.Request) {
	var in bulkInput
	if !httpx.Decode(w, r, &in) {
		return
	}
	var ids []string // each id once, in the order sent
	seen := map[string]bool{}
	for _, id := range in.TicketIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > MaxBulk {
		httpx.Error(w, r, http.StatusBadRequest, ErrBulkTickets)
		return
	}
	org := auth.OrgID(r.Context())
	out := make([]BulkResult, 0, len(ids))
	run := func(id, method, path string, handler http.HandlerFunc, body any) BulkResult {
		b, _ := json.Marshal(body)
		sub, _ := http.NewRequestWithContext(r.Context(), method, path, bytes.NewReader(b))
		sub.Header.Set("Accept", "application/json") // coded errors come back as JSON
		sub.SetPathValue("id", id)
		rec := httptest.NewRecorder() // the single-change handler's own answer, read back
		handler(rec, sub)
		res := BulkResult{ID: id, OK: rec.Code < 300, Status: rec.Code}
		if !res.OK {
			var e struct{ Code, Error string }
			if json.Unmarshal(rec.Body.Bytes(), &e) == nil && e.Error != "" {
				res.Code, res.Error = e.Code, e.Error
			} else {
				res.Error = http.StatusText(rec.Code)
			}
		}
		return res
	}
	// moves go in reverse, each to the top, so they land in the order they were sent
	order := ids
	if in.Action == "move" {
		order = slices.Clone(ids)
		slices.Reverse(order)
	}
	for _, id := range order {
		if IsKey(id) { // TL-123 as well as its id, as everywhere else
			if found, err := h.store.IDByKey(r.Context(), org, id); err == nil {
				id = found
			}
		}
		var res BulkResult
		switch in.Action {
		case "priority", "labels": // both need the ticket as it is: PATCH sends title and description too
			d, err := h.store.Get(r.Context(), org, id)
			if err != nil {
				res = BulkResult{ID: id, Status: http.StatusNotFound, Code: "not_found", Error: "not found"}
				break
			}
			if in.Action == "priority" {
				res = run(id, "PATCH", "/api/tickets/"+id, h.Update, map[string]any{"title": d.Title, "description": d.Description, "priority": in.Priority})
				break
			}
			labels := slices.DeleteFunc(slices.Clone(d.Labels), func(l string) bool {
				return slices.ContainsFunc(in.Remove, func(x string) bool { return equalFold(x, l) })
			})
			res = run(id, "PUT", "/api/tickets/"+id+"/labels", h.SetLabels, map[string]any{"labels": append(labels, in.Add...)})
		case "assign":
			res = run(id, "PUT", "/api/tickets/"+id+"/assignee", h.Assign, map[string]json.RawMessage{"assignee": rawOrNull(in.Assignee)})
		case "parent":
			res = run(id, "PUT", "/api/tickets/"+id+"/parent", h.SetParent, map[string]json.RawMessage{"parent_id": rawOrNull(in.ParentID)})
		case "move":
			res = run(id, "POST", "/api/tickets/"+id+"/move", h.Move, map[string]any{"column_id": in.ColumnID, "position": 0, "estimate": in.Estimate})
		case "resolve":
			res = run(id, "POST", "/api/tickets/"+id+"/resolve", h.Resolve, map[string]any{"resolution": in.Resolution, "reason": in.Reason})
		case "delete":
			res = run(id, "DELETE", "/api/tickets/"+id, h.Delete, nil)
		default:
			httpx.Error(w, r, http.StatusBadRequest, ErrBulkAction)
			return
		}
		out = append(out, res)
	}
	if in.Action == "move" {
		slices.Reverse(out) // answer in the order sent
	}
	// the org log (TL-669; paid plans): one bulk edit, how many it changed. Each ticket's own change is in
	// its activity already.
	changed := 0
	for _, res := range out {
		if res.OK {
			changed++
		}
	}
	if _, err := h.store.DB.Exec(r.Context(), `SELECT audit_org($1, 'tickets.bulk_edited', NULL, NULL, NULL, $2)`, org,
		map[string]any{"action": in.Action, "count": len(out), "changed": changed}); err != nil {
		logs.Errorf("tickets: bulk: audit: %v", err)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"results": out})
}

func rawOrNull(v json.RawMessage) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage("null")
	}
	return v
}

func equalFold(a, b string) bool { return bytes.EqualFold([]byte(a), []byte(b)) }

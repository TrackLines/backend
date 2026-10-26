package staff

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/tracklines/backend/internal/httpx"
)

// Count is one list's total and how many were added in the last 24 hours and 7 days.
type Count struct {
	Total int64 `json:"total"`
	Day   int64 `json:"day"`
	Week  int64 `json:"week"`
}

// SystemStats is the whole system in numbers (TL-574): totals only, never an id, a name or any content.
type SystemStats struct {
	Organizations   Count            `json:"organizations"`
	Plans           map[string]int64 `json:"plans"` // free, pro, enterprise
	Members         Count            `json:"members"`
	Projects        Count            `json:"projects"`
	Boards          Count            `json:"boards"`
	ArchivedBoards  int64            `json:"archived_boards"`
	Tickets         Count            `json:"tickets"`
	DoneTickets     int64            `json:"done_tickets"`
	Comments        Count            `json:"comments"`
	Attachments     Count            `json:"attachments"`
	AttachmentBytes int64            `json:"attachment_bytes"`
	APIKeys         map[string]int64 `json:"api_keys"` // unrevoked, by kind
	OpenSprints     int64            `json:"open_sprints"`
	LiveSessions    int64            `json:"live_sessions"`
	DatabaseBytes   int64            `json:"database_bytes"`
}

// Stats answers GET /api/staff/stats, cached in Valkey for a minute when there is one.
type Stats struct {
	DB *pgxpool.Pool
	VK valkey.Client
}

const statsKey = "tracklines:staff:stats"

func (s Stats) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.VK != nil {
		if hit, err := s.VK.Do(ctx, s.VK.B().Get().Key(statsKey).Build()).AsBytes(); err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(hit)
			return
		}
	}
	out, err := s.Count(ctx)
	if err != nil {
		if r.Context().Err() == nil {
			logs.Errorf("staff: stats: %v", err)
			httpx.Error(w, r, http.StatusInternalServerError, httpx.Code("stats_failed", "could not count"))
		}
		return
	}
	if b, err := json.Marshal(out); err == nil && s.VK != nil {
		_ = s.VK.Do(ctx, s.VK.B().Set().Key(statsKey).Value(string(b)).Ex(time.Minute).Build()).Error()
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Count reads every number in one query, then the live sessions from Valkey.
func (s Stats) Count(ctx context.Context) (SystemStats, error) {
	var out SystemStats
	var plans, keys []byte
	c := func(table, where string) string {
		if where != "" {
			where = " WHERE " + where
		}
		return `(SELECT jsonb_build_object('total', count(*), 'day', count(*) FILTER (WHERE created_at > now() - interval '1 day'),
			'week', count(*) FILTER (WHERE created_at > now() - interval '7 days')) FROM ` + table + where + `)`
	}
	// organizations have no table of their own (they're Clerk's): any org with a project or billing
	err := s.DB.QueryRow(ctx, `WITH orgs AS (
			SELECT org, min(created_at) AS created_at FROM (
				SELECT owner_clerk_id AS org, created_at FROM projects UNION ALL SELECT org_id, created_at FROM org_billing) o GROUP BY org),
		plans AS (
			SELECT CASE WHEN b.stripe_status IN ('active', 'trialing') THEN COALESCE(b.plan, 'enterprise') ELSE 'free' END AS plan
			FROM orgs LEFT JOIN org_billing b ON b.org_id = orgs.org)
		SELECT `+c("orgs", "")+`, (SELECT jsonb_object_agg(plan, n) FROM (SELECT plan, count(*) AS n FROM plans GROUP BY plan) p),
			`+c("users", "")+`, `+c("projects", "")+`, `+c("boards", "archived_at IS NULL")+`,
			(SELECT count(*) FROM boards WHERE archived_at IS NOT NULL),
			`+c("tickets", "")+`, (SELECT count(*) FROM tickets WHERE done_at IS NOT NULL),
			`+c("ticket_comments", "")+`, `+c("ticket_attachments", "")+`, (SELECT COALESCE(sum(size_bytes), 0) FROM ticket_attachments),
			(SELECT jsonb_object_agg(kind, n) FROM (SELECT COALESCE(kind, 'ai') AS kind, count(*) AS n FROM api_keys WHERE revoked_at IS NULL GROUP BY 1) k),
			(SELECT count(*) FROM sprints WHERE closed_at IS NULL), pg_database_size(current_database())`).Scan(
		&out.Organizations, &plans, &out.Members, &out.Projects, &out.Boards, &out.ArchivedBoards,
		&out.Tickets, &out.DoneTickets, &out.Comments, &out.Attachments, &out.AttachmentBytes, &keys, &out.OpenSprints, &out.DatabaseBytes)
	if err != nil {
		return out, err
	}
	out.Plans = map[string]int64{"free": 0, "pro": 0, "enterprise": 0}
	out.APIKeys = map[string]int64{}
	_ = json.Unmarshal(plans, &out.Plans)
	_ = json.Unmarshal(keys, &out.APIKeys)
	out.LiveSessions = s.liveSessions(ctx)
	return out, nil
}

// liveSessions counts running standups and refinements: their session keys, which are deleted when they
// end. ponytail: a SCAN over the live keys, fine at this size; a counter if live sessions get many.
func (s Stats) liveSessions(ctx context.Context) int64 {
	if s.VK == nil {
		return 0
	}
	var n int64
	cursor := uint64(0)
	for {
		e, err := s.VK.Do(ctx, s.VK.B().Scan().Cursor(cursor).Match("tracklines:live:*").Count(500).Build()).AsScanEntry()
		if err != nil {
			logs.Warnf("staff: live sessions: %v", err)
			return n
		}
		for _, k := range e.Elements {
			if session(k) {
				n++
			}
		}
		if cursor = e.Cursor; cursor == 0 {
			return n
		}
	}
}

// session: tracklines:live:session:<board> (standups) or tracklines:live:<kind>:<board|project>:<id>,
// not their :people sets or events.
func session(key string) bool {
	parts := strings.Split(strings.TrimPrefix(key, "tracklines:live:"), ":")
	switch {
	case len(parts) == 2 && parts[0] == "session":
		return true
	case len(parts) == 3 && parts[0] != "events" && (parts[1] == "board" || parts[1] == "project"):
		return true
	}
	return false
}

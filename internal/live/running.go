package live

import (
	"context"
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// RunningSession is a standup or refinement being hosted in the organization (TL-466).
type RunningSession struct {
	Kind        string   `json:"kind"` // standup (on a board) | refinement (on a project)
	ID          string   `json:"id"`   // the board's or project's
	Name        string   `json:"name"`
	ProjectID   string   `json:"project_id"`
	ProjectName string   `json:"project_name"`
	Host        string   `json:"host"`
	People      []string `json:"people"`
}

// Running is the organization's running sessions: its projects and live boards from Postgres, their
// session keys in one Valkey MGET, then the full session only for the few that are on.
func (s System) Running(ctx context.Context, org string) ([]RunningSession, error) {
	rows, err := s.Boards.DB.Query(ctx, `SELECT 'refinement', p.id::text, p.name, p.id::text, p.name FROM projects p WHERE p.owner_clerk_id = $1
		UNION ALL
		SELECT 'standup', b.id::text, b.name, p.id::text, p.name FROM boards b JOIN projects p ON p.id = b.project_id
		WHERE p.owner_clerk_id = $1 AND b.archived_at IS NULL
		ORDER BY 5, 1, 3`, org)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RunningSession, error) {
		var x RunningSession
		return x, r.Scan(&x.Kind, &x.ID, &x.Name, &x.ProjectID, &x.ProjectName)
	})
	out := []RunningSession{}
	if err != nil || len(all) == 0 {
		return out, err
	}
	scopes := make([]scope, len(all))
	keys := make([]string, len(all))
	for i, x := range all {
		if x.Kind == standup {
			scopes[i] = boardScope(standup, x.ID)
		} else {
			scopes[i] = projectScope(refinement, x.ID)
		}
		keys[i] = scopes[i].session
	}
	found, err := s.VK.Do(ctx, s.VK.B().Mget().Key(keys...).Build()).ToArray()
	if err != nil {
		return nil, err
	}
	for i, v := range found {
		if v.IsNil() {
			continue
		}
		h, err := s.hosted(ctx, scopes[i], all[i].Kind) // who's here, and ends it if its host has gone
		if errors.Is(err, ErrNoSession) {
			continue
		}
		if err != nil {
			return nil, err
		}
		all[i].Host, all[i].People = h.Host, h.People
		out = append(out, all[i])
	}
	return out, nil
}

// RunningHandler: GET /api/live/sessions, the caller's organization's running standups and refinements,
// so clients (the TUI's My work) can show them in one request instead of one per board.
func (s System) RunningHandler(w http.ResponseWriter, r *http.Request) {
	if s.VK == nil {
		httpx.JSON(w, http.StatusOK, []RunningSession{}) // live sessions off: none running
		return
	}
	out, err := s.Running(r.Context(), auth.OrgID(r.Context()))
	if err != nil {
		if !httpx.Canceled(err) {
			logs.Errorf("live: running sessions: %v", err)
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

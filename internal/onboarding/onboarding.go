// Package onboarding keeps organization setup separate from versioned, per-person guides.
package onboarding

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/projects"
)

// Publish a new version only with a matching frontend guide for newly released features.
// Deployment/build versions deliberately have no effect on guide availability.
const GuideID = "getting-started"
const GuideVersion = 1

type Guide struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	Available bool   `json:"available"`
}

type State struct {
	Admin       bool       `json:"admin"`
	Step        string     `json:"step"`
	ProjectID   *string    `json:"project_id"`
	BoardID     *string    `json:"board_id"`
	CompletedAt *time.Time `json:"completed_at"`
	Guide       Guide      `json:"guide"`
}

type Setup struct {
	Name       string `json:"name"`
	KeyPrefix  string `json:"key_prefix"`
	TemplateID string `json:"template_id"`
	BoardID    string `json:"board_id"` // adopt an imported board instead of creating anything
}

type System struct {
	DB     *pgxpool.Pool
	Admins organizations.Admins
}

var prefix = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,5}$`)
var errBoard = httpx.Code("onboarding_board", "choose a board in this organization")
var errSetup = httpx.Code("onboarding_incomplete", "finish organization setup before completing the guide")

// initialize remembers existing organizations as already set up. Called on the first status
// request, before a new admin leaves for an import, so importing can later resume this setup.
func (h System) initialize(ctx context.Context, org string) error {
	_, err := h.DB.Exec(ctx, `INSERT INTO org_onboarding (org_id, step, completed_at)
		SELECT $1, CASE WHEN EXISTS (SELECT 1 FROM projects WHERE owner_clerk_id = $1) THEN 'complete' ELSE 'project' END,
		CASE WHEN EXISTS (SELECT 1 FROM projects WHERE owner_clerk_id = $1) THEN now() ELSE NULL END
		ON CONFLICT DO NOTHING`, org)
	return err
}

func (h System) state(ctx context.Context, org, user string) (State, error) {
	return h.stateForGuide(ctx, org, user, Guide{ID: GuideID, Version: GuideVersion})
}

func (h System) stateForGuide(ctx context.Context, org, user string, guide Guide) (State, error) {
	s := State{Admin: true, Guide: guide}
	err := h.DB.QueryRow(ctx, `SELECT step, project_id::text, board_id::text, completed_at,
		NOT EXISTS (SELECT 1 FROM guide_completions WHERE org_id = $1 AND user_id = $2 AND guide_id = $3 AND version >= $4)
		FROM org_onboarding WHERE org_id = $1`, org, user, guide.ID, guide.Version).
		Scan(&s.Step, &s.ProjectID, &s.BoardID, &s.CompletedAt, &s.Guide.Available)
	return s, err
}

func (h System) Status(w http.ResponseWriter, r *http.Request) {
	org, user := auth.OrgID(r.Context()), ""
	var signedIn bool
	user, signedIn = auth.UserID(r.Context())
	if !signedIn || org == "" || auth.ViaAPIKey(r.Context()) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	admin, err := h.Admins.IsAdmin(r.Context(), org, user)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if !admin {
		httpx.JSON(w, http.StatusOK, State{Step: "complete"})
		return
	}
	if err := h.initialize(r.Context(), org); err != nil {
		writeErr(w, r, err)
		return
	}
	h.respond(w, r, org, user)
}

func (h System) respond(w http.ResponseWriter, r *http.Request, org, user string) {
	s, err := h.state(r.Context(), org, user)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.Admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in Setup
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err := h.initialize(r.Context(), org); err != nil {
		writeErr(w, r, err)
		return
	}
	limit, err := billing.ProjectLimit(r.Context(), h.DB, org)
	if err == nil {
		err = h.setup(r.Context(), org, in, limit)
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.respond(w, r, org, user)
}

func (h System) setup(ctx context.Context, org string, in Setup, limit int) error {
	return pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		// Same lock as ordinary project creation and imports: all paths honor the plan limit.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('projects:' || $1))`, org); err != nil {
			return err
		}
		var step string
		var board *string
		if err := tx.QueryRow(ctx, `SELECT step, board_id::text FROM org_onboarding WHERE org_id = $1 FOR UPDATE`, org).Scan(&step, &board); err != nil {
			return err
		}
		if step == "complete" || board != nil {
			return nil
		} // retry after response loss
		var projectID, boardID string
		if in.BoardID != "" {
			err := tx.QueryRow(ctx, `SELECT project_id::text, id::text FROM boards WHERE id::text = $1 AND owner_clerk_id = $2 AND archived_at IS NULL`, in.BoardID, org).Scan(&projectID, &boardID)
			if errors.Is(err, pgx.ErrNoRows) {
				return errBoard
			}
			if err != nil {
				return err
			}
		} else {
			in.Name = strings.TrimSpace(in.Name)
			in.KeyPrefix = strings.ToUpper(strings.TrimSpace(in.KeyPrefix))
			if in.Name == "" || len(in.Name) > 200 {
				return httpx.ErrNameRequired
			}
			if !prefix.MatchString(in.KeyPrefix) {
				return projects.ErrBadKeyPrefix
			}
			if in.TemplateID == "" {
				in.TemplateID = "builtin:simple"
			}
			template, err := boards.TemplateTx(ctx, tx, org, in.TemplateID)
			if errors.Is(err, boards.ErrNotFound) {
				return boards.ErrBadTemplate
			}
			if err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM projects WHERE owner_clerk_id = $1`, org).Scan(&count); err != nil {
				return err
			}
			if limit >= 0 && count >= limit {
				return projects.ErrLimit
			}
			if err := tx.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ($1, $2, $3) RETURNING id::text`, org, in.Name, in.KeyPrefix).Scan(&projectID); err != nil {
				return err
			}
			b, err := boards.CreateFromTx(ctx, tx, org, projectID, in.Name, "", template)
			if err != nil {
				return err
			}
			boardID = b.ID
		}
		_, err := tx.Exec(ctx, `UPDATE org_onboarding SET project_id = $2, board_id = $3, step = 'invite', updated_at = now() WHERE org_id = $1`, org, projectID, boardID)
		return err
	})
}

func (h System) Finish(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.Admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	tag, err := h.DB.Exec(r.Context(), `UPDATE org_onboarding SET step = 'complete', completed_at = COALESCE(completed_at, now()), updated_at = now()
		WHERE org_id = $1 AND (step = 'complete' OR EXISTS (SELECT 1 FROM boards WHERE id = board_id AND owner_clerk_id = $1 AND archived_at IS NULL))`, org)
	if err == nil && tag.RowsAffected() == 0 {
		err = errBoard
	}
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.respond(w, r, org, user)
}

func (h System) CompleteGuide(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.Admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.ID != GuideID || in.Version != GuideVersion {
		httpx.Error(w, r, http.StatusConflict, httpx.Code("guide_version", "refresh to load the current guide"))
		return
	}
	var complete bool
	err := h.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM org_onboarding WHERE org_id = $1 AND step = 'complete')`, org).Scan(&complete)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if !complete {
		writeErr(w, r, errSetup)
		return
	}
	_, err = h.DB.Exec(r.Context(), `INSERT INTO guide_completions (org_id, user_id, guide_id, version) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, org, user, in.ID, in.Version)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.respond(w, r, org, user)
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return
	}
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, projects.ErrLimit):
		httpx.Error(w, r, http.StatusPaymentRequired, httpx.Code("project_limit", "your plan's project limit is reached — upgrade for more"))
	case errors.As(err, &pg) && pg.Code == "23505":
		httpx.Error(w, r, http.StatusConflict, projects.ErrKeyPrefixTaken)
	case errors.Is(err, errBoard), errors.Is(err, errSetup), errors.Is(err, boards.ErrBadTemplate), errors.Is(err, projects.ErrBadKeyPrefix), errors.Is(err, httpx.ErrNameRequired):
		httpx.Error(w, r, http.StatusBadRequest, err)
	default:
		logs.Errorf("onboarding: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

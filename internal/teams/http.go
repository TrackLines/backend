package teams

import (
	"context"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

type membershipLister interface {
	List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)
}

type System struct {
	db          *pgxpool.Pool
	memberships membershipLister
	admins      organizations.Admins // who may name leaders and link boards
}

func NewSystem(db *pgxpool.Pool, admins organizations.Admins) System {
	return System{db: db, memberships: organizationmembership.NewClient(&clerk.ClientConfig{}), admins: admins}
}

// Member is one person on a team; leaders manage the team's boards.
type Member struct {
	UserID string `json:"user_id"`
	Leader bool   `json:"leader"`
}

type Team struct {
	ID          string `json:"id"`
	OrgID       string `json:"org_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func (h System) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `SELECT id, org_id, name, description,
		to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM teams WHERE org_id = $1 ORDER BY name`, auth.OrgID(r.Context()))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := make([]Team, 0)
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Name, &t.Description, &t.CreatedAt, &t.UpdatedAt); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		items = append(items, t)
	}
	if rows.Err() != nil {
		http.Error(w, "internal error", 500)
		return
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (h System) ProjectTeams(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `SELECT t.id,t.org_id,t.name,t.description,
		to_char(t.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		to_char(t.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM project_teams pt JOIN teams t ON t.id=pt.team_id
		JOIN projects p ON p.id=pt.project_id AND p.owner_clerk_id=pt.org_id
		WHERE pt.project_id=$1 AND pt.org_id=$2 ORDER BY t.name`, r.PathValue("projectID"), auth.OrgID(r.Context()))
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	defer rows.Close()
	items := make([]Team, 0)
	for rows.Next() {
		var t Team
		if err = rows.Scan(&t.ID, &t.OrgID, &t.Name, &t.Description, &t.CreatedAt, &t.UpdatedAt); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		items = append(items, t)
	}
	if rows.Err() != nil {
		http.Error(w, "internal error", 500)
		return
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (h System) Members(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `SELECT tm.user_clerk_id, tm.leader FROM team_members tm JOIN teams t ON t.id=tm.team_id WHERE t.id=$1 AND t.org_id=$2 ORDER BY tm.user_clerk_id`, r.PathValue("id"), auth.OrgID(r.Context()))
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	defer rows.Close()
	users := make([]Member, 0)
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.UserID, &m.Leader); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		users = append(users, m)
	}
	if rows.Err() != nil {
		http.Error(w, "internal error", 500)
		return
	}
	var exists bool
	if err = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM teams WHERE id=$1 AND org_id=$2)`, r.PathValue("id"), auth.OrgID(r.Context())).Scan(&exists); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	httpx.JSON(w, http.StatusOK, users)
}

func (h System) Create(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		http.Error(w, "name is required", 400)
		return
	}
	var t Team
	err := h.db.QueryRow(r.Context(), `INSERT INTO teams(org_id,name,description) VALUES($1,$2,$3) RETURNING id,org_id,name,description,to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),to_char(updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"')`, auth.OrgID(r.Context()), in.Name, in.Description).Scan(&t.ID, &t.OrgID, &t.Name, &t.Description, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	httpx.JSON(w, http.StatusCreated, t)
}

func (h System) Update(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		http.Error(w, "name is required", 400)
		return
	}
	tag, err := h.db.Exec(r.Context(), `UPDATE teams SET name=$3,description=$4,updated_at=now() WHERE id=$1 AND org_id=$2`, r.PathValue("id"), auth.OrgID(r.Context()), in.Name, in.Description)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	tag, err := h.db.Exec(r.Context(), `DELETE FROM teams WHERE id=$1 AND org_id=$2`, r.PathValue("id"), auth.OrgID(r.Context()))
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) AddMember(w http.ResponseWriter, r *http.Request)    { h.member(w, r, true) }
func (h System) RemoveMember(w http.ResponseWriter, r *http.Request) { h.member(w, r, false) }
func (h System) member(w http.ResponseWriter, r *http.Request, add bool) {
	orgID, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("userID")
	var err error
	if add {
		limit := int64(1)
		members, listErr := h.memberships.List(r.Context(), &organizationmembership.ListParams{
			OrganizationID: orgID,
			UserIDs:        []string{userID},
			ListParams:     clerk.ListParams{Limit: &limit},
		})
		if listErr != nil {
			logs.Errorf("teams: check organization membership: %v", listErr)
			http.Error(w, "could not verify organization membership", http.StatusBadGateway)
			return
		}
		isMember := false
		if members != nil {
			for _, membership := range members.OrganizationMemberships {
				if membership != nil && membership.PublicUserData != nil && membership.PublicUserData.UserID == userID {
					isMember = true
					break
				}
			}
		}
		if !isMember {
			http.Error(w, "user is not a member of this organization", http.StatusUnprocessableEntity)
			return
		}
		_, err = h.db.Exec(r.Context(), `INSERT INTO team_members(team_id,user_clerk_id) SELECT id,$3 FROM teams WHERE id=$1 AND org_id=$2 ON CONFLICT DO NOTHING`, r.PathValue("id"), orgID, userID)
	} else {
		_, err = h.db.Exec(r.Context(), `DELETE FROM team_members tm USING teams t WHERE tm.team_id=t.id AND t.id=$1 AND t.org_id=$2 AND tm.user_clerk_id=$3`, r.PathValue("id"), orgID, userID)
	}
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	var exists bool
	if err = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM teams WHERE id=$1 AND org_id=$2)`, r.PathValue("id"), orgID).Scan(&exists); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) AddProjectTeam(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	tag, err := h.db.Exec(r.Context(), `INSERT INTO project_teams(project_id,team_id,org_id)
		SELECT p.id,t.id,p.owner_clerk_id FROM projects p JOIN teams t ON t.id=$2 AND t.org_id=p.owner_clerk_id
		WHERE p.id=$1 AND p.owner_clerk_id=$3 ON CONFLICT DO NOTHING`, r.PathValue("projectID"), r.PathValue("teamID"), auth.OrgID(r.Context()))
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		err = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM project_teams WHERE project_id=$1 AND team_id=$2 AND org_id=$3)`, r.PathValue("projectID"), r.PathValue("teamID"), auth.OrgID(r.Context())).Scan(&exists)
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) RemoveProjectTeam(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	// a team off the project no longer runs that project's boards
	err := pgx.BeginFunc(r.Context(), h.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM project_teams WHERE project_id=$1 AND team_id=$2 AND org_id=$3`, r.PathValue("projectID"), r.PathValue("teamID"), auth.OrgID(r.Context()))
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(r.Context(), `UPDATE boards SET team_id = NULL, updated_at = now() WHERE project_id = $1 AND team_id = $2`, r.PathValue("projectID"), r.PathValue("teamID"))
		return err
	})
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

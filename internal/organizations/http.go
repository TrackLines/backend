package organizations

import (
	"context"
	"net/http"
	"strconv"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

const (
	defaultLimit = int64(100)
	maxLimit     = int64(500)
)

type membershipLister interface {
	List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)
}

type System struct {
	memberships membershipLister
	admins      Admins
}

func NewSystem(db *pgxpool.Pool) System {
	m := organizationmembership.NewClient(&clerk.ClientConfig{})
	return System{memberships: m, admins: Admins{DB: db, Orgs: organization.NewClient(&clerk.ClientConfig{}), Memberships: m}}
}

// AdminModel exposes the org-admin model to other packages (teams checks admins with it).
func (h System) AdminModel() Admins { return h.admins }

type Member struct {
	UserID     string  `json:"user_id"`
	FirstName  *string `json:"first_name,omitempty"`
	LastName   *string `json:"last_name,omitempty"`
	ImageURL   *string `json:"image_url,omitempty"`
	Identifier string  `json:"identifier"`
}

type MemberPage struct {
	Members    []Member `json:"members"`
	TotalCount int64    `json:"total_count"`
	Limit      int64    `json:"limit"`
	Offset     int64    `json:"offset"`
}

func (h System) Members(w http.ResponseWriter, r *http.Request) {
	orgID := auth.OrgID(r.Context())
	if orgID == "" {
		http.Error(w, "choose an organization first", http.StatusForbidden)
		return
	}
	limit, offset, ok := pagination(w, r)
	if !ok {
		return
	}
	list, err := h.memberships.List(r.Context(), &organizationmembership.ListParams{
		OrganizationID: orgID,
		ListParams: clerk.ListParams{
			Limit:  &limit,
			Offset: &offset,
		},
	})
	if err != nil {
		logs.Errorf("organizations: list memberships: %v", err)
		http.Error(w, "could not load organization members", http.StatusBadGateway)
		return
	}
	page := MemberPage{Members: make([]Member, 0, len(list.OrganizationMemberships)), TotalCount: list.TotalCount, Limit: limit, Offset: offset}
	for _, membership := range list.OrganizationMemberships {
		if membership == nil || membership.PublicUserData == nil {
			continue
		}
		user := membership.PublicUserData
		page.Members = append(page.Members, Member{
			UserID:     user.UserID,
			FirstName:  user.FirstName,
			LastName:   user.LastName,
			ImageURL:   user.ImageURL,
			Identifier: user.Identifier,
		})
	}
	httpx.JSON(w, http.StatusOK, page)
}

func pagination(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	limit, err := queryInt(r, "limit", defaultLimit)
	if err != nil || limit < 1 || limit > maxLimit {
		http.Error(w, "limit must be between 1 and 500", http.StatusBadRequest)
		return 0, 0, false
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || offset < 0 {
		http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
		return 0, 0, false
	}
	return limit, offset, true
}

func queryInt(r *http.Request, key string, fallback int64) (int64, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, nil
	}
	return strconv.ParseInt(value, 10, 64)
}

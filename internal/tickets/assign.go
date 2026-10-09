package tickets

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
)

var ErrUnknownAssignee = errors.New("assignee must be an active organization member or AI agent")

type membershipLister interface {
	List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)
}

// Assignee is a person or AI agent the owner can assign a ticket to.
type Assignee struct {
	ID    string `json:"id"`    // value stored in tickets.assigned_to
	Label string `json:"label"` // "You" or the AI agent's key name
	Kind  string `json:"kind"`  // "person" or "ai"
}

// Assign lets a caller assign a ticket to themself, an active organization member, or an active AI key,
// or clear assignment. The HTTP layer verifies organization membership with Clerk before passing member=true.
func (s Store) Assign(ctx context.Context, org, user, id string, assignee *string, member bool) (*Ticket, error) {
	t, err := boards.ScanTicket(s.DB.QueryRow(ctx, `UPDATE tickets t SET assigned_to = $3, updated_at = now()
		FROM projects p WHERE t.id = $1 AND p.id = t.project_id AND p.owner_clerk_id = $2
		AND ($3::text IS NULL OR $3::text = $4 OR $5::boolean OR EXISTS (
			SELECT 1 FROM api_keys k WHERE k.org_id = $2 AND k.name = $3
			AND k.kind = 'ai' AND k.revoked_at IS NULL
		)) RETURNING `+boards.TicketCols, id, org, assignee, user, member))
	if err == nil {
		return &t, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM tickets t JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1 AND p.owner_clerk_id = $2`, id, org).Scan(&ok); err != nil {
		return nil, notFound(err)
	}
	return nil, ErrUnknownAssignee
}

// Assignees lists org AI agent keys. Clerk organization members are added by the HTTP layer.
func (s Store) Assignees(ctx context.Context, org, user string) ([]Assignee, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT name FROM api_keys
		WHERE org_id = $1 AND kind = 'ai' AND revoked_at IS NULL ORDER BY name`, org)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := []Assignee{{ID: user, Label: "You", Kind: "person"}}
	for _, n := range names {
		out = append(out, Assignee{ID: n, Label: n, Kind: "ai"})
	}
	return out, err
}

// Assign: PUT /api/tickets/{id}/assignee {"assignee": "<id>" | null}.
func (h System) Assign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Assignee *string `json:"assignee"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	user, _ := auth.UserID(r.Context())
	org := auth.OrgID(r.Context())
	member := false
	if in.Assignee != nil && *in.Assignee != user && strings.HasPrefix(*in.Assignee, "user_") {
		var err error
		member, err = h.isOrganizationMember(r.Context(), org, *in.Assignee)
		if err != nil {
			logs.Errorf("tickets: check assignee organization membership: %v", err)
			http.Error(w, "could not verify organization membership", http.StatusBadGateway)
			return
		}
	}
	t, err := h.store.Assign(r.Context(), org, user, r.PathValue("id"), in.Assignee, member)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

// Assignees: GET /api/assignees.
func (h System) Assignees(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	org := auth.OrgID(r.Context())
	members, err := h.organizationAssignees(r.Context(), org, user)
	if err != nil {
		logs.Errorf("tickets: list assignable organization members: %v", err)
		http.Error(w, "could not load organization members", http.StatusBadGateway)
		return
	}
	out, err := h.store.Assignees(r.Context(), org, user)
	if err != nil {
		writeErr(w, err)
		return
	}
	seen := make(map[string]bool, len(members)+len(out))
	merged := make([]Assignee, 0, len(members)+len(out))
	for _, person := range members {
		if seen[person.ID] {
			continue
		}
		seen[person.ID] = true
		merged = append(merged, person)
	}
	for _, agent := range out {
		if seen[agent.ID] {
			continue
		}
		seen[agent.ID] = true
		merged = append(merged, agent)
	}
	httpx.JSON(w, http.StatusOK, merged)
}

func (h System) isOrganizationMember(ctx context.Context, org, user string) (bool, error) {
	limit := int64(1)
	list, err := h.memberships.List(ctx, &organizationmembership.ListParams{
		OrganizationID: org,
		UserIDs:        []string{user},
		ListParams:     clerk.ListParams{Limit: &limit},
	})
	if err != nil {
		return false, err
	}
	if list == nil {
		return false, nil
	}
	for _, membership := range list.OrganizationMemberships {
		if membership != nil && membership.PublicUserData != nil && membership.PublicUserData.UserID == user {
			return true, nil
		}
	}
	return false, nil
}

func (h System) organizationAssignees(ctx context.Context, org, user string) ([]Assignee, error) {
	const limit int64 = 500
	var out []Assignee
	source := h.memberships
	if h.assigneeMembers != nil {
		source = h.assigneeMembers
	}
	for offset := int64(0); ; offset += limit {
		list, err := source.List(ctx, &organizationmembership.ListParams{
			OrganizationID: org,
			ListParams:     clerk.ListParams{Limit: ptr(limit), Offset: ptr(offset)},
		})
		if err != nil {
			return nil, err
		}
		if list == nil {
			break
		}
		for _, membership := range list.OrganizationMemberships {
			if membership == nil || membership.PublicUserData == nil || membership.PublicUserData.UserID == "" {
				continue
			}
			person := membership.PublicUserData
			label := strings.TrimSpace(strings.Join([]string{value(person.FirstName), value(person.LastName)}, " "))
			if label == "" {
				label = person.Identifier
			}
			if person.UserID == user {
				label = "You"
			}
			if label == "" {
				label = person.UserID
			}
			out = append(out, Assignee{ID: person.UserID, Label: label, Kind: "person"})
		}
		if int64(len(list.OrganizationMemberships)) < limit || offset+limit >= list.TotalCount {
			break
		}
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

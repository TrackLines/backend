package billing

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// The launch offer's staff side (TL-778): every organization with whether it's been sent the offer
// and taken it, and sending it to the ones staff pick. There's no blanket send and no tester list:
// an org only gets the offer when staff choose it.

// OrgLister lists Clerk organizations (organization.Client in production).
type OrgLister interface {
	List(ctx context.Context, params *organization.ListParams) (*clerk.OrganizationList, error)
}

// MemberLister lists an organization's Clerk members (organizationmembership.Client in production).
type MemberLister interface {
	List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)
}

type LaunchOfferStaff struct {
	DB      *pgxpool.Pool
	On      func() bool // the launch-offer flag: sending is refused while it's off
	Orgs    OrgLister
	Members MemberLister
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
	Push    notifications.Notifier // devices, as well as the inbox; nil: inbox only
}

// Offer statuses, for the admin page.
const (
	OfferNotSent   = "not_sent"
	OfferSent      = "sent"
	OfferClaimed   = "claimed"   // on the free year now
	OfferConverted = "converted" // the year ended (or a card was added) and it's paying
	OfferEnded     = "ended"     // cancelled: back on Free
	OfferPaid      = "paid"      // already paying, so it can't be sent
)

// OfferOrg is one organization on the admin page.
type OfferOrg struct {
	OrgID       string     `json:"org_id"`
	Name        string     `json:"name"`
	CreatedAt   time.Time  `json:"created_at"`
	Members     int64      `json:"members"`
	Plan        string     `json:"plan"`
	Status      string     `json:"status"`
	OfferedAt   *time.Time `json:"offered_at"`
	ClaimedAt   *time.Time `json:"claimed_at"`
	ClaimedBy   *string    `json:"claimed_by"`
	TrialEndsAt *time.Time `json:"trial_ends_at"`
}

type offerRow struct {
	offeredAt, claimedAt, trialEndsAt *time.Time
	claimedBy, subscription           *string
}

type billingRow struct {
	plan, status, subscription *string
}

func (b billingRow) live() bool {
	return b.status != nil && (*b.status == "active" || *b.status == "trialing")
}

func (b billingRow) planName() string {
	if !b.live() {
		return Free
	}
	if b.plan == nil {
		return Enterprise
	}
	return *b.plan
}

// statusOf works out where an org is with the offer.
func statusOf(o *offerRow, b billingRow) string {
	switch {
	case o == nil && b.live():
		return OfferPaid
	case o == nil:
		return OfferNotSent
	case o.claimedAt == nil:
		return OfferSent
	case b.subscription != nil && o.subscription != nil && *b.subscription == *o.subscription && b.status != nil && *b.status == "trialing":
		return OfferClaimed
	case b.live():
		return OfferConverted
	}
	return OfferEnded
}

// List: GET /api/staff/launch-offers. Newest organizations first.
func (s LaunchOfferStaff) List(w http.ResponseWriter, r *http.Request) {
	out, err := s.list(r.Context())
	if err != nil {
		logs.Errorf("billing: launch offers list: %v", err)
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("billing_unavailable", "billing unavailable"))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"organizations": out, "flag_on": s.On != nil && s.On()})
}

func (s LaunchOfferStaff) list(ctx context.Context) ([]OfferOrg, error) {
	offers := map[string]*offerRow{}
	rows, err := s.DB.Query(ctx, `SELECT org_id, offered_at, claimed_at, claimed_by, trial_ends_at, subscription_id FROM launch_offers`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var org string
		o := &offerRow{}
		if err := rows.Scan(&org, &o.offeredAt, &o.claimedAt, &o.claimedBy, &o.trialEndsAt, &o.subscription); err != nil {
			rows.Close()
			return nil, err
		}
		offers[org] = o
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	bills := map[string]billingRow{}
	rows, err = s.DB.Query(ctx, `SELECT org_id, plan, stripe_status, stripe_subscription_id FROM org_billing`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var org string
		var b billingRow
		if err := rows.Scan(&org, &b.plan, &b.status, &b.subscription); err != nil {
			rows.Close()
			return nil, err
		}
		bills[org] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	orgs, err := allOrgs(ctx, s.Orgs)
	if err != nil {
		return nil, err
	}
	out := []OfferOrg{}
	for _, org := range orgs {
		o, b := offers[org.ID], bills[org.ID]
		row := OfferOrg{OrgID: org.ID, Name: org.Name, CreatedAt: time.UnixMilli(org.CreatedAt).UTC(), Plan: b.planName(), Status: statusOf(o, b)}
		if org.MembersCount != nil {
			row.Members = *org.MembersCount
		}
		if o != nil {
			row.OfferedAt, row.ClaimedAt, row.ClaimedBy, row.TrialEndsAt = o.offeredAt, o.claimedAt, o.claimedBy, o.trialEndsAt
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// allOrgs pages through every Clerk organization, with member counts.
func allOrgs(ctx context.Context, orgs OrgLister) ([]*clerk.Organization, error) {
	var out []*clerk.Organization
	limit, count := int64(100), true
	for offset := int64(0); ; offset += limit {
		page := offset
		list, err := orgs.List(ctx, &organization.ListParams{IncludeMembersCount: &count, ListParams: clerk.ListParams{Limit: &limit, Offset: &page}})
		if err != nil {
			return out, err
		}
		if list == nil {
			return out, nil
		}
		for _, org := range list.Organizations {
			if org != nil {
				out = append(out, org)
			}
		}
		if int64(len(list.Organizations)) < limit || offset+limit >= list.TotalCount {
			return out, nil
		}
	}
}

// Skipped is an org that wasn't sent the offer, and why.
type Skipped struct {
	OrgID  string `json:"org_id"`
	Reason string `json:"reason"` // already_sent | paid | failed
}

const (
	offerTextAdmin  = "Your organization has been offered a free year of Pro. Claim it from the plan card in Settings."
	offerTextMember = "Your organization has been offered a free year of Pro. An admin can claim it from the plan card in Settings."
)

// Send: POST /api/staff/launch-offers {"org_ids": [...]}. Each chosen org is sent the offer once, and
// its members are told: admins can claim it, members are told an admin can.
func (s LaunchOfferStaff) Send(w http.ResponseWriter, r *http.Request) {
	if s.On == nil || !s.On() {
		httpx.Error(w, r, http.StatusConflict, httpx.Code("launch_offer_off", "turn on the launch-offer flag before sending the offer"))
		return
	}
	var in struct {
		OrgIDs []string `json:"org_ids"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	seen := map[string]bool{}
	var orgs []string
	for _, id := range in.OrgIDs {
		id = strings.TrimSpace(id)
		if !strings.HasPrefix(id, "org_") || seen[id] {
			continue
		}
		seen[id] = true
		orgs = append(orgs, id)
	}
	if len(orgs) == 0 || len(orgs) > 500 {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("bad_org_ids", "choose between 1 and 500 organizations"))
		return
	}
	staffUser, _ := auth.UserID(r.Context())
	sent, skipped := []string{}, []Skipped{}
	for _, org := range orgs {
		reason, err := s.sendOne(r.Context(), org, staffUser)
		if err != nil {
			logs.Errorf("billing: launch offer send %s: %v", org, err)
			reason = "failed"
		}
		if reason != "" {
			skipped = append(skipped, Skipped{OrgID: org, Reason: reason})
			continue
		}
		sent = append(sent, org)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"sent": sent, "skipped": skipped})
}

// sendOne records the offer and tells the org's members; reason says why it was skipped.
func (s LaunchOfferStaff) sendOne(ctx context.Context, org, staffUser string) (reason string, err error) {
	paid, err := IsPaid(ctx, s.DB, org)
	if err != nil {
		return "", err
	}
	if paid {
		return "paid", nil
	}
	members, err := s.members(ctx, org)
	if err != nil {
		return "", err
	}
	var admins, others []string
	for _, m := range members {
		admin := false
		if s.IsAdmin != nil {
			if admin, err = s.IsAdmin(ctx, org, m); err != nil {
				return "", err
			}
		}
		if admin {
			admins = append(admins, m)
		} else {
			others = append(others, m)
		}
	}
	sentNow := false
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO launch_offers (org_id, offered_by) VALUES ($1, $2) ON CONFLICT DO NOTHING`, org, staffUser)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		sentNow = true
		// the inbox, with the link to the claim; written with the offer so neither is half done
		for _, group := range []struct {
			to   []string
			text string
		}{{admins, offerTextAdmin}, {others, offerTextMember}} {
			if len(group.to) == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO notifications (org_id, user_id, kind, text, link) SELECT $1, u, 'billing', $3, $4 FROM unnest($2::text[]) AS u`,
				org, group.to, group.text, LaunchOfferLink); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !sentNow {
		return "already_sent", nil
	}
	if s.Push != nil { // devices: best effort, the inbox already has it
		notifications.Send(ctx, s.Push, org, admins, notifications.Event{Kind: "billing", Actor: "TrackLin.es", Text: "is giving your organization a free year of Pro. Claim it in Settings.", Link: LaunchOfferLink})
		notifications.Send(ctx, s.Push, org, others, notifications.Event{Kind: "billing", Actor: "TrackLin.es", Text: "is giving your organization a free year of Pro. An admin can claim it.", Link: LaunchOfferLink})
	}
	return "", nil
}

// members: the org's people (Clerk user ids).
func (s LaunchOfferStaff) members(ctx context.Context, org string) ([]string, error) {
	if s.Members == nil {
		return nil, nil
	}
	limit := int64(500)
	var out []string
	for offset := int64(0); ; offset += limit {
		page := offset
		list, err := s.Members.List(ctx, &organizationmembership.ListParams{OrganizationID: org, ListParams: clerk.ListParams{Limit: &limit, Offset: &page}})
		if err != nil {
			return nil, err
		}
		if list == nil {
			break
		}
		for _, m := range list.OrganizationMemberships {
			if m != nil && m.PublicUserData != nil && strings.HasPrefix(m.PublicUserData.UserID, "user_") {
				out = append(out, m.PublicUserData.UserID)
			}
		}
		if int64(len(list.OrganizationMemberships)) < limit || offset+limit >= list.TotalCount {
			break
		}
	}
	return out, nil
}

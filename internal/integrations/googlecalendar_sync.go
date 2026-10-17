package integrations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sync window (TL-406): a week back (so "back on Monday" is still known) to 90 days ahead.
const (
	syncBack  = 7 * 24 * time.Hour
	syncAhead = 90 * 24 * time.Hour
)

type memberLister interface {
	List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)
}

type userLister interface {
	List(ctx context.Context, params *user.ListParams) (*clerk.UserList, error)
}

// CalendarSync copies when people are away from Google Calendar into time_away, for every org that
// connected it: each member's Out of office events (matched by their email in the Workspace domain)
// and the shared holidays calendar's days off. Each run replaces the window, so moved or cancelled
// leave is corrected next time.
type CalendarSync struct {
	DB      *pgxpool.Pool
	Google  *GoogleCalendar
	Members memberLister
	Users   userLister // every verified email of each member (one may be GitHub's, another Google's); nil: sign-in and on-file only
	now     func() time.Time
}

func (s CalendarSync) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Run syncs every connected org each interval until ctx is cancelled.
// ponytail: every instance syncs; the window is replaced in one transaction per person, so a second
// instance only repeats the work. Add an advisory lock if the API quota starts to matter.
func (s CalendarSync) Run(ctx context.Context, every time.Duration) {
	if s.Google == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.All(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// All syncs every connected org; one org's trouble is recorded on it and doesn't stop the rest.
func (s CalendarSync) All(ctx context.Context) {
	rows, err := s.DB.Query(ctx, `SELECT org_id FROM calendar_connections`)
	if err != nil {
		logs.Errorf("google calendar: sync: %v", err)
		return
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		logs.Errorf("google calendar: sync: %v", err)
		return
	}
	for _, org := range orgs {
		if ctx.Err() != nil {
			return
		}
		if err := s.Org(ctx, org); err != nil {
			logs.Warnf("google calendar: sync %s: %v", org, err)
		}
	}
}

// Org syncs one org and records how it went (last_sync_at, last_sync_error) on its connection.
func (s CalendarSync) Org(ctx context.Context, org string) error {
	c, err := getCalendar(ctx, s.DB, org)
	if err != nil {
		return err
	}
	syncErr := s.org(ctx, org, c)
	message := ""
	if syncErr != nil {
		message = "Couldn't sync; we'll try again within the hour" // details go to the log, not admins' screens (audit)
		var refused CalendarError
		if errors.As(syncErr, &refused) {
			message = refused.Message
		} else {
			logs.Warnf("google calendar: sync %s: %v", org, syncErr)
		}
	}
	if _, err := s.DB.Exec(ctx, `UPDATE calendar_connections SET last_sync_at = now(), last_sync_error = $2 WHERE org_id = $1`, org, message); err != nil {
		return err
	}
	return syncErr
}

func (s CalendarSync) org(ctx context.Context, org string, c *CalendarConnection) error {
	now := s.clock()
	from, to := now.Add(-syncBack), now.Add(syncAhead)
	people, err := s.memberEmails(ctx, org)
	if err != nil {
		return fmt.Errorf("members: %w", err)
	}
	// keep only what's useful: leave that ended over a week ago goes (TL-411, the privacy policy says so)
	if _, err := s.DB.Exec(ctx, `DELETE FROM time_away WHERE org_id = $1 AND ends_at <= $2`, org, from); err != nil {
		return err
	}
	// someone who left the org takes their time away with them
	ids := make([]string, 0, len(people))
	for id := range people {
		ids = append(ids, id)
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM time_away WHERE org_id = $1 AND user_id IS NOT NULL AND NOT (user_id = ANY($2))`, org, ids); err != nil {
		return err
	}
	failed := 0
	for user, emails := range people {
		email := inDomain(emails, c.Domain)
		if email == "" {
			continue // no Google account in this Workspace
		}
		events, zone, err := s.Google.Events(ctx, email, "primary", from, to, true)
		var refused CalendarError
		if errors.As(err, &refused) && refused.Code == "invalid_grant" {
			continue // not a user in the Workspace (or suspended): nothing to read
		}
		if errors.As(err, &refused) && refused.Code == "unauthorized_client" {
			return err // access withdrawn: stop, and say so on the connection
		}
		if err != nil {
			failed++
			continue
		}
		if err := s.replace(ctx, org, &user, from, to, spans(events, zone)); err != nil {
			return err
		}
	}
	if c.HolidaysCalendar != "" {
		events, zone, err := s.Google.Events(ctx, c.AdminEmail, c.HolidaysCalendar, from, to, false)
		if err != nil {
			return fmt.Errorf("holidays calendar: %w", err)
		}
		if err := s.replace(ctx, org, nil, from, to, wholeDays(spans(events, zone))); err != nil { // days off: all-day events only
			return err
		}
	} else if err := s.replace(ctx, org, nil, from, to, nil); err != nil { // holidays calendar removed
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d people's calendars couldn't be read; they'll be tried again next sync", failed)
	}
	return nil
}

// span is one stretch of time away, in UTC.
type span struct {
	start, end time.Time
	allDay     bool
	id         string
}

// spans turns events into time away: cancelled ones dropped; all-day events are whole days in the
// calendar's time zone (Google's end date is the day after the last day off).
func spans(events []CalendarEvent, zone string) []span {
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		loc = time.UTC
	}
	var out []span
	for _, e := range events {
		if e.Status == "cancelled" {
			continue
		}
		var sp span
		switch {
		case e.Start.Date != "" && e.End.Date != "":
			start, err1 := time.ParseInLocation(time.DateOnly, e.Start.Date, loc)
			end, err2 := time.ParseInLocation(time.DateOnly, e.End.Date, loc)
			if err1 != nil || err2 != nil {
				continue
			}
			sp = span{start.UTC(), end.UTC(), true, e.ID}
		case e.Start.DateTime != "" && e.End.DateTime != "":
			start, err1 := time.Parse(time.RFC3339, e.Start.DateTime)
			end, err2 := time.Parse(time.RFC3339, e.End.DateTime)
			if err1 != nil || err2 != nil {
				continue
			}
			sp = span{start.UTC(), end.UTC(), false, e.ID}
		default:
			continue
		}
		if sp.end.After(sp.start) {
			out = append(out, sp)
		}
	}
	return out
}

// wholeDays keeps whole days: a shared calendar's meetings aren't company days off.
func wholeDays(list []span) []span {
	var out []span
	for _, sp := range list {
		if sp.allDay {
			out = append(out, sp)
		}
	}
	return out
}

// replace swaps one person's (or, user nil, the company's) time away in the window for what Google
// has now, in one transaction so readers never see it half done.
func (s CalendarSync) replace(ctx context.Context, org string, user *string, from, to time.Time, list []span) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// one sync of an org at a time (hourly, Sync now, every instance), so two can't both insert (audit)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('time-away:' || $1))`, org); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM time_away WHERE org_id = $1 AND user_id IS NOT DISTINCT FROM $2 AND ends_at > $3 AND starts_at < $4`, org, user, from, to); err != nil {
		return err
	}
	for _, sp := range list {
		if _, err := tx.Exec(ctx, `INSERT INTO time_away (org_id, user_id, starts_at, ends_at, all_day, source_id) VALUES ($1, $2, $3, $4, $5, $6)`,
			org, user, sp.start, sp.end, sp.allDay, sp.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// memberEmails: each org member's email addresses, lower case: their Clerk sign-in identifier, every
// verified address on their Clerk account (someone signed in with GitHub may have their Workspace
// address as a second one), and the one we have on file.
func (s CalendarSync) memberEmails(ctx context.Context, org string) (map[string][]string, error) {
	limit := int64(500)
	out := map[string][]string{}
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
			if m == nil || m.PublicUserData == nil || m.PublicUserData.UserID == "" {
				continue
			}
			out[m.PublicUserData.UserID] = append(out[m.PublicUserData.UserID], strings.ToLower(m.PublicUserData.Identifier))
		}
		if int64(len(list.OrganizationMemberships)) < limit || offset+limit >= list.TotalCount {
			break
		}
	}
	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	if s.Users != nil {
		for start := 0; start < len(ids); start += 100 { // Clerk takes up to 100 user ids a request
			batch := ids[start:min(start+100, len(ids))]
			limit := int64(len(batch))
			list, err := s.Users.List(ctx, &user.ListParams{UserIDs: batch, ListParams: clerk.ListParams{Limit: &limit}})
			if err != nil {
				return nil, fmt.Errorf("user emails: %w", err)
			}
			for _, u := range list.Users {
				for _, e := range u.EmailAddresses {
					if e != nil && e.Verification != nil && e.Verification.Status == "verified" {
						out[u.ID] = append(out[u.ID], strings.ToLower(e.EmailAddress))
					}
				}
			}
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT clerk_id, lower(email) FROM users WHERE clerk_id = ANY($1) AND email <> ''`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, email string
		if rows.Scan(&id, &email) == nil {
			out[id] = append(out[id], email)
		}
	}
	return out, rows.Err()
}

// inDomain: the first of emails at domain, or "".
func inDomain(emails []string, domain string) string {
	for _, e := range emails {
		if strings.HasSuffix(e, "@"+domain) && strings.Count(e, "@") == 1 {
			return e
		}
	}
	return ""
}

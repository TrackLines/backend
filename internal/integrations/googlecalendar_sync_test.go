package integrations

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeMembers is Clerk's membership list: user id → sign-in identifier.
type fakeMembers map[string]string

func (f fakeMembers) List(_ context.Context, _ *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	out := &clerk.OrganizationMembershipList{}
	for id, identifier := range f {
		out.OrganizationMemberships = append(out.OrganizationMemberships, &clerk.OrganizationMembership{
			PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: id, Identifier: identifier}})
	}
	out.TotalCount = int64(len(out.OrganizationMemberships))
	return out, nil
}

// fakeUsers is Clerk's user list: user id → their email addresses, true when verified.
type fakeUsers map[string]map[string]bool

func (f fakeUsers) List(_ context.Context, params *user.ListParams) (*clerk.UserList, error) {
	out := &clerk.UserList{}
	for _, id := range params.UserIDs {
		emails, ok := f[id]
		if !ok {
			continue
		}
		u := &clerk.User{ID: id}
		for e, verified := range emails {
			status := "unverified"
			if verified {
				status = "verified"
			}
			u.EmailAddresses = append(u.EmailAddresses, &clerk.EmailAddress{EmailAddress: e, Verification: &clerk.Verification{Status: status}})
		}
		out.Users = append(out.Users, u)
	}
	out.TotalCount = int64(len(out.Users))
	return out, nil
}

func allDay(id, start, end string) map[string]any {
	return map[string]any{"id": id, "status": "confirmed", "eventType": "outOfOffice", "summary": "Secret trip to Lisbon",
		"start": map[string]any{"date": start}, "end": map[string]any{"date": end}}
}

// TL-406: Out of office events and company days off become time away (only when, never what), and
// each sync corrects the last.
func TestGoogleCalendarSync(t *testing.T) {
	ctx := context.Background()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+dbURL[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM calendar_connections WHERE org_id IN ('org_away', 'org_away2');
		DELETE FROM users WHERE clerk_id IN ('u_ava', 'u_ben');
		INSERT INTO users (clerk_id, email) VALUES ('u_ben', 'Ben@Acme.test');
		INSERT INTO calendar_connections (org_id, domain, admin_email, holidays_calendar, connected_by) VALUES
			('org_away', 'acme.test', 'boss@acme.test', 'holidays@group.v.calendar.google.com', 'test'),
			('org_away2', 'other.test', 'x@other.test', '', 'test');
		INSERT INTO time_away (org_id, user_id, starts_at, ends_at, source_id) VALUES ('org_away2', 'u_x', now(), now() + interval '1 day', 'keep')`); err != nil {
		t.Fatal(err)
	}

	fake := &fakeCalendar{delegated: true, users: map[string]bool{"boss@acme.test": true, "ava@acme.test": true, "ben@acme.test": true, "eve@acme.test": true, "fay@acme.test": true},
		zones: map[string]string{"primary:ava@acme.test": "Europe/London"},
		events: map[string][]map[string]any{
			// Ava: a week off (all-day, London), a timed afternoon off, and a cancelled day
			"primary:ava@acme.test": {
				allDay("ava-week", "2026-10-19", "2026-10-24"),
				{"id": "ava-pm", "status": "confirmed", "eventType": "outOfOffice", "start": map[string]any{"dateTime": "2026-10-27T13:00:00+00:00"}, "end": map[string]any{"dateTime": "2026-10-27T17:00:00+00:00"}},
				{"id": "ava-gone", "status": "cancelled", "start": map[string]any{"date": "2026-10-28"}, "end": map[string]any{"date": "2026-10-29"}},
			},
			"primary:ben@acme.test": {allDay("ben-day", "2026-10-30", "2026-10-31")},
			"primary:eve@acme.test": {allDay("eve-day", "2026-10-18", "2026-10-19")},
			"primary:fay@acme.test": {allDay("fay-day", "2026-10-18", "2026-10-19")},
			"holidays@group.v.calendar.google.com": {allDay("xmas", "2026-12-25", "2026-12-27"), // a timed meeting on the shared calendar isn't a day off (audit)
				{"id": "meeting", "status": "confirmed", "start": map[string]any{"dateTime": "2026-10-21T10:00:00Z"}, "end": map[string]any{"dateTime": "2026-10-21T11:00:00Z"}}},
		}}
	g := newTestCalendar(t, db, fake)
	members := fakeMembers{"u_ava": "ava@acme.test", "u_ben": "ben@gmail.test", "u_cara": "cara@elsewhere.test", "u_dan": "dan@acme.test",
		"u_eve": "eve@github.test", "u_fay": "fay@github.test"}
	// Eve signs in with GitHub; her Workspace address is a second, verified email on her account. Fay's isn't verified.
	users := fakeUsers{"u_eve": {"eve@github.test": true, "Eve@Acme.test": true}, "u_fay": {"fay@github.test": true, "fay@acme.test": false}}
	sync := CalendarSync{DB: db, Google: g, Members: members, Users: users, now: func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }}

	away := func(org string) []string {
		rows, _ := db.Query(ctx, `SELECT COALESCE(user_id, 'company'), to_char(starts_at AT TIME ZONE 'UTC', 'MM-DD HH24:MI'), to_char(ends_at AT TIME ZONE 'UTC', 'MM-DD HH24:MI'), all_day FROM time_away WHERE org_id = $1`, org)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var who, start, end string
			var day bool
			rows.Scan(&who, &start, &end, &day)
			kind := "timed"
			if day {
				kind = "day"
			}
			out = append(out, who+" "+start+"→"+end+" "+kind)
		}
		sort.Strings(out)
		return out
	}

	// leave that ended over a week before the sync is old news: deleted, not kept forever
	db.Exec(ctx, `INSERT INTO time_away (org_id, user_id, starts_at, ends_at, source_id) VALUES ('org_away', 'u_ava', '2026-09-01', '2026-09-05', 'old')`)
	if err := sync.Org(ctx, "org_away"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"company 12-25 00:00→12-27 00:00 day", // a company day off: everyone (calendar in UTC)
		"u_ava 10-18 23:00→10-23 23:00 day",   // whole days in London (BST): 19th to 23rd inclusive
		"u_ava 10-27 13:00→10-27 17:00 timed", // the cancelled day isn't there
		"u_ben 10-30 00:00→10-31 00:00 day",   // matched by the email we have for him, not his Gmail sign-in
		"u_eve 10-18 00:00→10-19 00:00 day",   // a verified second email (she signs in with GitHub); Fay's unverified one isn't used
	}
	if got := away("org_away"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("time away:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Cara has no address in the domain: never asked for; Dan isn't a Workspace user: skipped quietly
	for _, a := range fake.asked {
		if strings.HasPrefix(a, "cara@") {
			t.Error("asked Google about someone outside the domain")
		}
	}
	var lastErr string
	var synced *time.Time
	db.QueryRow(ctx, `SELECT last_sync_error, last_sync_at FROM calendar_connections WHERE org_id = 'org_away'`).Scan(&lastErr, &synced)
	if lastErr != "" || synced == nil {
		t.Errorf("sync recorded: %q %v", lastErr, synced)
	}
	// only when, never what: there's nowhere to keep a title
	var cols string
	db.QueryRow(ctx, `SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns WHERE table_name = 'time_away'`).Scan(&cols)
	if cols != "all_day,ends_at,id,org_id,source_id,starts_at,user_id" {
		t.Errorf("time_away keeps more than when: %s", cols)
	}

	// next sync: Ava moved her week, Ben's day was deleted, Ben left the org, the holiday calendar gone
	fake.events["primary:ava@acme.test"] = []map[string]any{allDay("ava-week", "2026-11-02", "2026-11-07")}
	delete(members, "u_ben")
	db.Exec(ctx, `UPDATE calendar_connections SET holidays_calendar = '' WHERE org_id = 'org_away'`)
	if err := sync.Org(ctx, "org_away"); err != nil {
		t.Fatal(err)
	}
	if got := away("org_away"); strings.Join(got, "\n") != "u_ava 11-02 00:00→11-07 00:00 day\nu_eve 10-18 00:00→10-19 00:00 day" { // GMT again in November; Eve unchanged
		t.Errorf("after changes: %v", got)
	}

	// access withdrawn: the sync stops and says why on the connection, and keeps what it had
	fake.delegated = false
	g.tokens = map[string]calendarToken{}
	if err := sync.Org(ctx, "org_away"); err == nil {
		t.Error("withdrawn access synced")
	}
	db.QueryRow(ctx, `SELECT last_sync_error FROM calendar_connections WHERE org_id = 'org_away'`).Scan(&lastErr)
	if !strings.Contains(lastErr, "Manage Domain Wide Delegation") || len(away("org_away")) != 2 {
		t.Errorf("after access withdrawn: %q %v", lastErr, away("org_away"))
	}

	// another org's time away is its own; disconnecting deletes an org's time away
	if got := away("org_away2"); len(got) != 1 {
		t.Errorf("another org: %v", got)
	}
	db.Exec(ctx, `DELETE FROM calendar_connections WHERE org_id = 'org_away'`)
	if got := away("org_away"); len(got) != 0 {
		t.Errorf("after disconnect: %v", got)
	}
}

func TestSpans(t *testing.T) {
	// a bad zone falls back to UTC; an event with no dates, or that ends before it starts, is dropped
	got := spans([]CalendarEvent{{ID: "a", Start: struct {
		Date     string `json:"date"`
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	}{Date: "2026-10-20"}, End: struct {
		Date     string `json:"date"`
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	}{Date: "2026-10-20"}}, {ID: "b"}}, "Not/AZone")
	if len(got) != 0 {
		t.Errorf("spans: %+v", got)
	}
}

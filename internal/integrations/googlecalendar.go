package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/httpx"
)

// Google Calendar (TL-404): who's on holiday. A Workspace admin lets our service account read their
// domain's calendars (domain-wide delegation, one read-only scope); we read only Out of office events
// and, optionally, a shared calendar of company days off.
const (
	GoogleCalendarProvider = "google-calendar"
	calendarScope          = "https://www.googleapis.com/auth/calendar.readonly"
	calendarAPI            = "https://www.googleapis.com/calendar/v3"
)

// GoogleCalendar reads calendars as the people in a connected domain. nil: the server has no
// service account, so Google Calendar isn't available.
type GoogleCalendar struct {
	DB     *pgxpool.Pool
	Client *http.Client
	API    string // calendarAPI; a test server in tests

	sa  *serviceAccount
	now func() time.Time

	mu     sync.Mutex
	tokens map[string]calendarToken // by the user it acts as
}

type calendarToken struct {
	value string
	exp   time.Time
}

// NewGoogleCalendar uses the service account the Chat app uses. No credentials: nil.
func NewGoogleCalendar(db *pgxpool.Pool, credentials string) (*GoogleCalendar, error) {
	if strings.TrimSpace(credentials) == "" {
		return nil, nil
	}
	sa, err := parseServiceAccount(credentials)
	if err != nil {
		return nil, fmt.Errorf("google calendar: %w", err)
	}
	return &GoogleCalendar{DB: db, Client: externalClient(15 * time.Second), API: calendarAPI, sa: sa, tokens: map[string]calendarToken{}}, nil
}

// ClientID is what a Workspace admin authorizes for domain-wide delegation.
func (g *GoogleCalendar) ClientID() string { return g.sa.clientID }

func (g *GoogleCalendar) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// CalendarError is Google's refusal, in words an admin can act on. Code: the token endpoint's
// error (unauthorized_client: access not granted; invalid_grant: no such user in the domain).
type CalendarError struct{ Message, Code string }

func (e CalendarError) Error() string { return e.Message }

// tokenFor gets an access token acting as subject (a user in a domain that delegated to us).
func (g *GoogleCalendar) tokenFor(ctx context.Context, subject string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if t, ok := g.tokens[subject]; ok && now.Before(t.exp.Add(-time.Minute)) {
		return t.value, nil
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: g.sa.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", g.sa.keyID))
	if err != nil {
		return "", err
	}
	assertion, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": g.sa.email, "sub": subject, "scope": calendarScope, "aud": g.sa.tokenURL, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}).Serialize()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.sa.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := g.do(req, &out); err != nil {
		return "", err
	}
	g.tokens[subject] = calendarToken{out.AccessToken, now.Add(time.Duration(out.ExpiresIn) * time.Second)}
	return out.AccessToken, nil
}

// do sends req and decodes the JSON answer; Google's errors become CalendarErrors.
func (g *GoogleCalendar) do(req *http.Request, out any) error {
	res, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		return calendarRefusal(res.StatusCode, payload)
	}
	return json.Unmarshal(payload, out)
}

// calendarRefusal explains why Google said no. The token endpoint answers {"error": "...",
// "error_description": "..."}; the Calendar API {"error": {"code", "message"}}.
func calendarRefusal(status int, payload []byte) error {
	var token struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(payload, &token) == nil && token.Error != "" {
		switch token.Error {
		case "unauthorized_client", "access_denied":
			return CalendarError{"Google hasn't allowed TrackLin.es to read calendars yet. As a super admin, in admin.google.com → Security → Access and data control → API controls → Manage Domain Wide Delegation, add our client ID with the calendar scope (it can take a few minutes to apply).", token.Error}
		case "invalid_grant":
			return CalendarError{"Google doesn't know that user. Check the admin email is a user in this Google Workspace.", token.Error}
		}
		return CalendarError{"Google refused: " + token.Error, token.Error}
	}
	var api struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &api) == nil && api.Error.Message != "" {
		if status == http.StatusNotFound {
			return CalendarError{Message: "Google couldn't find that calendar, or it isn't shared with the admin."}
		}
		return CalendarError{Message: "Google Calendar refused: " + api.Error.Message}
	}
	return CalendarError{Message: fmt.Sprintf("Google Calendar answered %d", status)}
}

// CalendarEvent is the part of an event we use: when, never what (TL-404 keeps no titles).
type CalendarEvent struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // confirmed, tentative, cancelled
	EventType string `json:"eventType"`
	Start     struct {
		Date     string `json:"date"`     // all-day: 2026-10-20
		DateTime string `json:"dateTime"` // timed: RFC 3339
		TimeZone string `json:"timeZone"`
	} `json:"start"`
	End struct {
		Date     string `json:"date"`
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	} `json:"end"`
}

// Events lists calendarID's events between from and to, reading as subject, with the calendar's
// time zone (all-day events are days in it). outOfOffice: only Out of office events. Recurring
// events come as their single occurrences.
func (g *GoogleCalendar) Events(ctx context.Context, subject, calendarID string, from, to time.Time, outOfOffice bool) ([]CalendarEvent, string, error) {
	token, err := g.tokenFor(ctx, subject)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{"timeMin": {from.UTC().Format(time.RFC3339)}, "timeMax": {to.UTC().Format(time.RFC3339)},
		"singleEvents": {"true"}, "maxResults": {"250"}, "fields": {"items(id,status,eventType,start,end),nextPageToken,timeZone"}}
	if outOfOffice {
		q.Set("eventTypes", "outOfOffice")
	}
	var all []CalendarEvent
	var zone string
	for page := 0; page < 20; page++ { // ponytail: 5,000 events per calendar per sync is plenty for leave
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.API+"/calendars/"+url.PathEscape(calendarID)+"/events?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		var out struct {
			Items         []CalendarEvent `json:"items"`
			NextPageToken string          `json:"nextPageToken"`
			TimeZone      string          `json:"timeZone"`
		}
		if err := g.do(req, &out); err != nil {
			return nil, "", err
		}
		all, zone = append(all, out.Items...), out.TimeZone
		if out.NextPageToken == "" {
			break
		}
		q.Set("pageToken", out.NextPageToken)
	}
	return all, zone, nil
}

// Check proves the connection works: the admin's own Out of office events can be read, and so can
// the holidays calendar if there is one.
func (g *GoogleCalendar) Check(ctx context.Context, adminEmail, holidays string) error {
	now := g.clock()
	if _, _, err := g.Events(ctx, adminEmail, "primary", now, now.AddDate(0, 0, 1), true); err != nil {
		return err
	}
	if holidays != "" {
		if _, _, err := g.Events(ctx, adminEmail, holidays, now, now.AddDate(0, 0, 1), false); err != nil {
			return err
		}
	}
	return nil
}

// --- the org's connection ---

// CalendarConnection is an org's Google Calendar set-up.
type CalendarConnection struct {
	Domain           string  `json:"domain"`
	AdminEmail       string  `json:"admin_email,omitempty"` // admins only
	HolidaysCalendar string  `json:"holidays_calendar"`
	ConnectedAt      string  `json:"connected_at"`
	LastSyncAt       *string `json:"last_sync_at"`
	LastSyncError    string  `json:"last_sync_error"`
}

var errNoCalendar = errors.New("google calendar not connected")

func getCalendar(ctx context.Context, db *pgxpool.Pool, org string) (*CalendarConnection, error) {
	var c CalendarConnection
	err := db.QueryRow(ctx, `SELECT domain, admin_email, holidays_calendar,
		to_char(connected_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		to_char(last_sync_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), last_sync_error
		FROM calendar_connections WHERE org_id = $1`, org).Scan(&c.Domain, &c.AdminEmail, &c.HolidaysCalendar, &c.ConnectedAt, &c.LastSyncAt, &c.LastSyncError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoCalendar
	}
	return &c, err
}

var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// calendarInput checks an admin's set-up: a domain, an admin email in it, and maybe a calendar id.
func calendarInput(domain, adminEmail, holidays string) (string, string, string, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	adminEmail = strings.ToLower(strings.TrimSpace(adminEmail))
	holidays = strings.TrimSpace(holidays)
	switch {
	case len(domain) > 253 || !domainPattern.MatchString(domain):
		return "", "", "", httpx.Code("calendar_domain_required", "enter your Google Workspace domain, e.g. example.com")
	case !strings.HasSuffix(adminEmail, "@"+domain) || strings.Count(adminEmail, "@") != 1 || len(adminEmail) <= len(domain)+1:
		return "", "", "", httpx.Code("calendar_admin_email", "the admin email must be an address at "+domain)
	case len(holidays) > 300 || strings.ContainsAny(holidays, " \t\r\n/"):
		return "", "", "", httpx.Code("calendar_holidays_id", "the holidays calendar should be a calendar ID, e.g. en.uk#holiday@group.v.calendar.google.com")
	// a public holiday calendar or a shared (group) calendar, never someone's own calendar (audit)
	case holidays != "" && !strings.HasSuffix(holidays, "#holiday@group.v.calendar.google.com") && !strings.HasSuffix(holidays, "@group.calendar.google.com"):
		return "", "", "", httpx.Code("calendar_holidays_kind", "the holidays calendar must be a public holiday calendar (…#holiday@group.v.calendar.google.com) or a shared calendar (…@group.calendar.google.com)")
	}
	return domain, adminEmail, holidays, nil
}

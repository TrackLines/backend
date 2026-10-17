package integrations

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
)

// fakeCalendar is Google's token endpoint and Calendar API: delegated says whether the Workspace
// admin has authorized our client ID; users are the domain's people.
type fakeCalendar struct {
	mu        sync.Mutex
	delegated bool
	users     map[string]bool
	asked     []string                    // "sub scope" for each token
	events    map[string][]map[string]any // by calendar id; "primary:<user>" is that user's own
	zones     map[string]string           // a calendar's time zone (UTC if not set)
	query     []string                    // each events request's query
}

func (f *fakeCalendar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		tok, err := jwt.ParseSigned(r.Form.Get("assertion"), []jose.SignatureAlgorithm{jose.RS256})
		var claims struct {
			Sub   string `json:"sub"`
			Scope string `json:"scope"`
		}
		if err != nil || tok.UnsafeClaimsWithoutVerification(&claims) != nil {
			http.Error(w, "bad assertion", 400)
			return
		}
		f.asked = append(f.asked, claims.Sub+" "+claims.Scope)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case !f.delegated:
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens using this method"}`))
		case !f.users[claims.Sub]:
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid email or User ID"}`))
		default:
			w.Write([]byte(`{"access_token":"at-` + claims.Sub + `","expires_in":3600}`))
		}
		return
	}
	calendar, ok := strings.CutPrefix(strings.TrimSuffix(r.URL.EscapedPath(), "/events"), "/calendars/")
	if !ok || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") {
		http.Error(w, "no", 401)
		return
	}
	f.query = append(f.query, r.URL.RawQuery)
	if calendar == "primary" {
		if _, own := f.events["primary:"+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")]; own {
			calendar = "primary:" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
		}
	}
	items, known := f.events[calendar]
	if !known {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"code":404,"message":"Not Found"}}`))
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"items": items, "timeZone": f.zones[calendar]})
}

func newTestCalendar(t *testing.T, db *pgxpool.Pool, fake *fakeCalendar) *GoogleCalendar {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	creds, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "tracklines@p.iam.gserviceaccount.com", "private_key_id": "k1",
		"client_id": "1234567890", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": srv.URL + "/token"})
	g, err := NewGoogleCalendar(db, string(creds))
	if err != nil {
		t.Fatal(err)
	}
	g.Client, g.API = srv.Client(), srv.URL // production uses externalClient and googleapis.com
	return g
}

// TL-405: org admins connect Google Calendar once Google lets us in; members only see that it is.
func TestGoogleCalendarConnection(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM calendar_connections WHERE org_id IN ('org_cal', 'org_cal2');
		DELETE FROM org_admins WHERE org_id IN ('org_cal', 'org_cal2');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_cal', 'cal_admin', 'test'), ('org_cal2', 'other_admin', 'test')`); err != nil {
		t.Fatal(err)
	}

	fake := &fakeCalendar{users: map[string]bool{"boss@acme.test": true}, events: map[string][]map[string]any{
		"primary": {}, "en.uk%23holiday@group.v.calendar.google.com": {},
	}}
	h := CalendarSystem{DB: db, Admins: organizations.Admins{DB: db}, Google: newTestCalendar(t, db, fake)}
	session := func(user, org string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
			c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
			c.ActiveOrganizationID = org
			return c
		}())
	}
	admin, member := session("cal_admin", "org_cal"), session("cal_member", "org_cal")
	call := func(hf http.HandlerFunc, c context.Context, method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	get := func(c context.Context) map[string]any {
		var out map[string]any
		json.Unmarshal(call(h.Get, c, "GET", "").Body.Bytes(), &out)
		return out
	}
	body := `{"domain":" Acme.test ","admin_email":"Boss@acme.test","holidays_calendar":"en.uk#holiday@group.v.calendar.google.com"}`

	// before: available, not connected, and what to paste into Google
	if g := get(member); g["available"] != true || g["connected"] != false || g["client_id"] != "1234567890" || g["scope"] != calendarScope {
		t.Fatalf("before connecting: %v", g)
	}

	// only admins connect; the input is checked; Google must let us in first
	if rec := call(h.Connect, member, "PUT", body); rec.Code != 403 {
		t.Errorf("member connecting: %d", rec.Code)
	}
	if rec := call(h.Connect, auth.WithAPIKey(ctx, "cal_admin", "org_cal", "k", "ai"), "PUT", body); rec.Code != 403 {
		t.Errorf("an API key connecting: %d", rec.Code)
	}
	for _, bad := range []string{`{"domain":"acme","admin_email":"boss@acme"}`, `{"domain":"acme.test","admin_email":"boss@other.test"}`, `{"domain":"acme.test","admin_email":"@acme.test"}`, `{"domain":"acme.test","admin_email":"boss@acme.test","holidays_calendar":"a b"}`} {
		if rec := call(h.Connect, admin, "PUT", bad); rec.Code != 400 {
			t.Errorf("bad input %s: %d", bad, rec.Code)
		}
	}
	rec := call(h.Connect, admin, "PUT", body)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Manage Domain Wide Delegation") {
		t.Errorf("not delegated yet: %d %s", rec.Code, rec.Body)
	}
	if get(admin)["connected"] != false {
		t.Error("a refused connection was saved")
	}
	fake.delegated = true
	if rec := call(h.Connect, admin, "PUT", `{"domain":"acme.test","admin_email":"ghost@acme.test"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "doesn't know that user") {
		t.Errorf("unknown admin: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.Connect, admin, "PUT", `{"domain":"acme.test","admin_email":"boss@acme.test","holidays_calendar":"missing@group.v.calendar.google.com"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "couldn't find that calendar") {
		t.Errorf("unknown holidays calendar: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.Connect, admin, "PUT", body); rec.Code != 204 {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	// it acts as the admin, with the read-only calendar scope, and asks only for Out of office events
	if last := fake.asked[len(fake.asked)-1]; last != "boss@acme.test "+calendarScope {
		t.Errorf("token asked for: %q", last)
	}
	if !strings.Contains(fake.query[len(fake.query)-2], "eventTypes=outOfOffice") {
		t.Errorf("primary calendar query: %s", fake.query[len(fake.query)-2])
	}

	// members see it's connected, not the admin's email; admins see everything
	if g := get(member); g["connected"] != true || g["connection"].(map[string]any)["domain"] != "acme.test" || g["connection"].(map[string]any)["admin_email"] != nil {
		t.Errorf("member view: %v", g)
	}
	if c := get(admin)["connection"].(map[string]any); c["admin_email"] != "boss@acme.test" || c["holidays_calendar"] != "en.uk#holiday@group.v.calendar.google.com" {
		t.Errorf("admin view: %v", c)
	}
	// another org isn't connected by it
	if g := get(session("other_admin", "org_cal2")); g["connected"] != false {
		t.Errorf("another org: %v", g)
	}

	// Sync now (TL-406): admins only; answers with how it went
	h.Sync = &CalendarSync{DB: db, Google: h.Google, Members: fakeMembers{}}
	if rec := call(h.SyncNow, member, "POST", ""); rec.Code != 403 {
		t.Errorf("member syncing: %d", rec.Code)
	}
	if rec := call(h.SyncNow, admin, "POST", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"last_sync_at":"20`) || !strings.Contains(rec.Body.String(), `"last_sync_error":""`) {
		t.Errorf("sync now: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.SyncNow, session("other_admin", "org_cal2"), "POST", ""); rec.Code != 404 {
		t.Errorf("sync now, not connected: %d", rec.Code)
	}
	h.Sync = nil

	// test again: fine, then access withdrawn in Google
	if rec := call(h.Test, admin, "POST", ""); rec.Code != 204 {
		t.Errorf("test: %d %s", rec.Code, rec.Body)
	}
	fake.delegated = false
	h.Google.tokens = map[string]calendarToken{} // a withdrawn grant shows once the cached token expires
	if rec := call(h.Test, admin, "POST", ""); rec.Code != 400 {
		t.Errorf("test after access withdrawn: %d", rec.Code)
	}
	if rec := call(h.Test, member, "POST", ""); rec.Code != 403 {
		t.Errorf("member testing: %d", rec.Code)
	}

	// disconnect: admins only
	if rec := call(h.Disconnect, member, "DELETE", ""); rec.Code != 403 {
		t.Errorf("member disconnecting: %d", rec.Code)
	}
	if rec := call(h.Disconnect, admin, "DELETE", ""); rec.Code != 204 || get(admin)["connected"] != false {
		t.Errorf("disconnect: %d", rec.Code)
	}

	// no service account: not available
	h.Google = nil
	if g := get(admin); g["available"] != false || g["client_id"] != nil {
		t.Errorf("no service account: %v", g)
	}
	if rec := call(h.Connect, admin, "PUT", body); rec.Code != 503 {
		t.Errorf("connect with no service account: %d", rec.Code)
	}
	if g, err := NewGoogleCalendar(db, " "); g != nil || err != nil {
		t.Errorf("no credentials: %v %v", g, err)
	}
}

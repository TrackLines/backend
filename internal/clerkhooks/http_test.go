package clerkhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/apikeys"
)

var key = []byte("test-signing-key")
var secret = "whsec_" + base64.StdEncoding.EncodeToString(key)

func sign(k []byte, id string, t time.Time, body string) string {
	mac := hmac.New(sha256.New, k)
	fmt.Fprintf(mac, "%s.%d.%s", id, t.Unix(), body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ts := fmt.Sprint(now.Unix())
	body := `{"type":"x"}`
	good := sign(key, "msg_1", now, body)
	if err := Verify([]byte(body), "msg_1", ts, good, secret, now); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// several signatures (secret rotation): any match is enough
	if err := Verify([]byte(body), "msg_1", ts, sign([]byte("old"), "msg_1", now, body)+" "+good, secret, now); err != nil {
		t.Fatalf("multiple: %v", err)
	}
	for name, c := range map[string]struct{ body, id, ts, sig, secret string }{
		"wrong secret":  {body, "msg_1", ts, sign([]byte("other"), "msg_1", now, body), secret},
		"tampered body": {body + " ", "msg_1", ts, good, secret},
		"other id":      {body, "msg_2", ts, good, secret},
		"stale":         {body, "msg_1", fmt.Sprint(now.Add(-10 * time.Minute).Unix()), sign(key, "msg_1", now.Add(-10*time.Minute), body), secret},
		"no secret":     {body, "msg_1", ts, good, ""},
		"no id":         {body, "", ts, good, secret},
		"malformed":     {body, "msg_1", ts, "garbage", secret},
	} {
		if Verify([]byte(c.body), c.id, c.ts, c.sig, c.secret, now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWebhook(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// alice is in org_a and org_b, bob in org_a, carol in org_c
	for _, u := range []string{"alice", "bob", "carol"} {
		exec(`INSERT INTO users (clerk_id, email) VALUES ($1, $1 || '@x.y')`, u)
	}
	keys := apikeys.Store{DB: db}
	mint := func(owner, org string) string {
		plain, _, err := keys.Create(ctx, owner, org, owner+"-"+org, apikeys.KindAI, nil, nil, apikeys.DefaultDays, false)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}
	aliceA, aliceB, bobA, carolC := mint("alice", "org_a"), mint("alice", "org_b"), mint("bob", "org_a"), mint("carol", "org_c")
	teams := map[string]string{}
	for _, org := range []string{"org_a", "org_b", "org_c"} {
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 't') RETURNING id`, org).Scan(&id); err != nil {
			t.Fatal(err)
		}
		teams[org] = id
	}
	for _, m := range [][2]string{{"org_a", "alice"}, {"org_b", "alice"}, {"org_a", "bob"}, {"org_c", "carol"}} {
		exec(`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, $2, 'test')`, m[0], m[1])
		exec(`INSERT INTO team_members (team_id, user_clerk_id, leader) VALUES ($1, $2, true)`, teams[m[0]], m[1])
	}

	now := time.Now()
	h := NewSystem(db, secret).Webhook(func() time.Time { return now })
	n := 0
	send := func(body string) int {
		n++
		id := fmt.Sprintf("msg_%d", n)
		req := httptest.NewRequest("POST", "/api/webhooks/clerk", strings.NewReader(body))
		req.Header.Set("svix-id", id)
		req.Header.Set("svix-timestamp", fmt.Sprint(now.Unix()))
		req.Header.Set("svix-signature", sign(key, id, now, body))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	live := func(plain string) bool {
		_, _, _, _, _, err := keys.Owner(ctx, plain)
		return err == nil
	}
	count := func(q string, args ...any) int {
		var c int
		if err := db.QueryRow(ctx, q, args...).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	member := func(org, user string) bool {
		return count(`SELECT count(*) FROM org_admins WHERE org_id = $1 AND user_clerk_id = $2`, org, user)+
			count(`SELECT count(*) FROM team_members WHERE team_id = $1 AND user_clerk_id = $2`, teams[org], user) > 0
	}

	// bad signature is refused and changes nothing
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"type":"user.deleted","data":{"id":"bob"}}`))
	req.Header.Set("svix-id", "msg_x")
	req.Header.Set("svix-timestamp", fmt.Sprint(now.Unix()))
	req.Header.Set("svix-signature", "v1,AAAA")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest || !live(bobA) {
		t.Fatalf("forged: %d", rec.Code)
	}
	// unconfigured secret: 503
	rec = httptest.NewRecorder()
	NewSystem(db, "").Webhook(time.Now)(rec, httptest.NewRequest("POST", "/", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", rec.Code)
	}

	// alice leaves org_a: only her org_a rows and key go; replay is a no-op
	leave := `{"type":"organizationMembership.deleted","data":{"organization":{"id":"org_a"},"public_user_data":{"user_id":"alice"}}}`
	for range 2 {
		if code := send(leave); code != http.StatusNoContent {
			t.Fatalf("membership deleted: %d", code)
		}
	}
	if live(aliceA) || member("org_a", "alice") {
		t.Fatal("alice still has org_a access")
	}
	if !live(aliceB) || !member("org_b", "alice") || !live(bobA) || !member("org_a", "bob") {
		t.Fatal("membership removal touched other orgs or users")
	}

	// personal data for bob and alice (TL-594): bob's goes when he's deleted, alice's stays
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_a', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, assigned_to, title, position) VALUES ($1, 'bob', 'bob', 'his', 0) RETURNING id`, pid).Scan(&tid)
	exec(`INSERT INTO ticket_comments (ticket_id, body, author) VALUES ($1, 'hi', 'bob')`, tid)
	exec(`INSERT INTO calendar_connections (org_id, domain, admin_email, connected_by) VALUES ('org_a', 'x.y', 'alice@x.y', 'alice')`)
	exec(`INSERT INTO org_billing (org_id, billing_email) VALUES ('org_a', 'Bob@x.y'), ('org_b', 'alice@x.y')`)
	for i, u := range []string{"bob", "alice"} {
		exec(`INSERT INTO notifications (org_id, user_id, kind, text) VALUES ('org_a', $1, 'mention', 'x')`, u)
		exec(`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES ($1, 'https://push/' || $1, 'k', 'a')`, u)
		exec(`INSERT INTO ticket_mentions (org_id, user_id, ticket_id) VALUES ('org_a', $1, $2)`, u, tid)
		exec(`INSERT INTO ticket_watch_mutes (org_id, user_id, ticket_id) VALUES ('org_a', $1, $2)`, u, tid)
		exec(`INSERT INTO time_away (org_id, user_id, starts_at, ends_at, source_id) VALUES ('org_a', $1, now(), now() + interval '1 day', $1)`, u)
		exec(`INSERT INTO project_members (project_id, user_clerk_id, invited_by) VALUES ($1, $2, 'carol')`, pid, u)
		exec(`INSERT INTO invitations (org_id, clerk_invitation_id, email_address, scope, invited_by, status, accepted_user_clerk_id, expires_at)
			VALUES ('org_a', $1, $2 || '@x.y', 'organization', 'carol', 'accepted', $2, now())`, fmt.Sprint("inv_", i), u)
	}
	personal := func(u string) int {
		total := 0
		for _, q := range []string{
			`SELECT count(*) FROM users WHERE clerk_id = $1`,
			`SELECT count(*) FROM notifications WHERE user_id = $1`,
			`SELECT count(*) FROM push_subscriptions WHERE user_id = $1`,
			`SELECT count(*) FROM ticket_mentions WHERE user_id = $1`,
			`SELECT count(*) FROM ticket_watch_mutes WHERE user_id = $1`,
			`SELECT count(*) FROM time_away WHERE user_id = $1`,
			`SELECT count(*) FROM project_members WHERE user_clerk_id = $1`,
			`SELECT count(*) FROM invitations WHERE accepted_user_clerk_id = $1`,
			`SELECT count(*) FROM org_billing WHERE lower(billing_email) = $1 || '@x.y'`,
		} {
			total += count(q, u)
		}
		return total
	}
	if personal("bob") != 9 || personal("alice") != 9 {
		t.Fatalf("seeded: bob %d, alice %d", personal("bob"), personal("alice"))
	}

	// bob is deleted: his rows, keys and personal data go everywhere; a replay is a no-op
	for range 2 {
		if code := send(`{"type":"user.deleted","data":{"id":"bob"}}`); code != http.StatusNoContent || live(bobA) || member("org_a", "bob") {
			t.Fatalf("user deleted: %d", code)
		}
	}
	if n := personal("bob"); n != 0 {
		t.Fatalf("bob's personal data left: %d rows", n)
	}
	if !live(aliceB) || !live(carolC) || personal("alice") != 9 {
		t.Fatal("user deletion touched other users")
	}
	// his work stays, under his id (his open ticket is unassigned: TL-608); his revoked key stays on record
	if count(`SELECT count(*) FROM tickets WHERE created_by = 'bob' AND assigned_to IS NULL`) != 1 ||
		count(`SELECT count(*) FROM ticket_comments WHERE author = 'bob'`) != 1 ||
		count(`SELECT count(*) FROM api_keys WHERE owner_clerk_id = 'bob' AND revoked_at IS NOT NULL`) != 1 {
		t.Fatal("bob's tickets, comments or revoked key went")
	}

	// org_c is deleted: its admins, teams and keys go; org_b is untouched
	exec(`INSERT INTO projects (owner_clerk_id, name) VALUES ('org_c', 'kept')`)
	for range 2 {
		if code := send(`{"type":"organization.deleted","data":{"id":"org_c"}}`); code != http.StatusNoContent {
			t.Fatalf("org deleted: %d", code)
		}
	}
	if live(carolC) || count(`SELECT count(*) FROM teams WHERE org_id = 'org_c'`) != 0 || count(`SELECT count(*) FROM org_admins WHERE org_id = 'org_c'`) != 0 {
		t.Fatal("org_c not cleaned up")
	}
	if count(`SELECT count(*) FROM projects WHERE owner_clerk_id = 'org_c'`) != 1 {
		t.Fatal("org_c projects should be kept (for 30 days)")
	}
	if count(`SELECT count(*) FROM deleted_orgs WHERE org_id = 'org_c'`) != 1 {
		t.Fatal("org_c's deletion wasn't recorded for the 30-day purge")
	}
	if !live(aliceB) || !member("org_b", "alice") {
		t.Fatal("org deletion touched another org")
	}

	// other event types are acknowledged and ignored
	if code := send(`{"type":"user.created","data":{"id":"dave"}}`); code != http.StatusNoContent {
		t.Fatalf("ignored type: %d", code)
	}
}

// fakeOrgs records the member cap Clerk is told for each organization.
type fakeOrgs map[string]int64

func (f fakeOrgs) Update(_ context.Context, id string, p *organization.UpdateParams) (*clerk.Organization, error) {
	f[id] = *p.MaxAllowedMemberships
	return &clerk.Organization{ID: id}, nil
}

// TL-418: a new organization starts on Free, so Clerk is told it holds up to 20 members.
func TestNewOrganizationGetsTheMemberCap(t *testing.T) {
	db, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(context.Background(), `DELETE FROM org_billing WHERE org_id = 'org_new'`)
	orgs := fakeOrgs{}
	s := NewSystem(db, secret)
	s.ClerkOrgs = orgs
	now := time.Now()
	body := `{"type":"organization.created","data":{"id":"org_new"}}`
	req := httptest.NewRequest("POST", "/api/webhooks/clerk", strings.NewReader(body))
	req.Header.Set("svix-id", "msg_new_org")
	req.Header.Set("svix-timestamp", fmt.Sprint(now.Unix()))
	req.Header.Set("svix-signature", sign(key, "msg_new_org", now, body))
	rec := httptest.NewRecorder()
	s.Webhook(func() time.Time { return now })(rec, req)
	if rec.Code != 204 || orgs["org_new"] != 20 {
		t.Errorf("new organization: %d, cap %d", rec.Code, orgs["org_new"])
	}
}

type joins struct {
	got []string
	err error
}

func (j *joins) CompleteFor(_ context.Context, org, user string) (int, error) {
	j.got = append(j.got, org+"/"+user)
	return 1, j.err
}

// TL-518: someone joining an org has their invitations completed; a failure is a 503 so Svix retries.
func TestMembershipCreatedCompletesInvitations(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	invites := &joins{}
	var forgot []string // TL-520: the org's cached member lists go on any membership change
	forget := func(_ context.Context, org string) { forgot = append(forgot, org) }
	h := System{DB: testDB(t), Secret: secret, Invites: invites, Forget: forget}.Webhook(func() time.Time { return now })
	send := func(body string) int {
		req := httptest.NewRequest("POST", "/api/webhooks/clerk", strings.NewReader(body))
		req.Header.Set("svix-id", "msg_join")
		req.Header.Set("svix-timestamp", fmt.Sprint(now.Unix()))
		req.Header.Set("svix-signature", sign(key, "msg_join", now, body))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	join := `{"type":"organizationMembership.created","data":{"organization":{"id":"org_a"},"public_user_data":{"user_id":"dana"}}}`
	if code := send(join); code != http.StatusNoContent || len(invites.got) != 1 || invites.got[0] != "org_a/dana" {
		t.Fatalf("join: %d %v", code, invites.got)
	}
	if code := send(`{"type":"organizationMembership.updated","data":{"organization":{"id":"org_b"},"public_user_data":{"user_id":"dana"}}}`); code != http.StatusNoContent {
		t.Fatalf("updated: %d", code)
	}
	if code := send(`{"type":"user.deleted","data":{"id":"nobody"}}`); code != http.StatusNoContent {
		t.Fatalf("user.deleted: %d", code)
	}
	if strings.Join(forgot, ",") != "org_a,org_b" {
		t.Fatalf("forgot %v, want org_a,org_b", forgot)
	}
	if code := send(`{"type":"organizationMembership.created","data":{"organization":{"id":""},"public_user_data":{"user_id":"dana"}}}`); code != http.StatusNoContent || len(invites.got) != 1 {
		t.Fatalf("no org: %d %v", code, invites.got)
	}
	invites.err = errors.New("clerk down")
	if code := send(join); code != http.StatusServiceUnavailable {
		t.Fatalf("failure should be retried: %d", code)
	}
}

// testDB is the migrated test database.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// TL-669: joining and leaving are in a paid org's audit log, by the system (Clerk told us); not a Free org's
func TestMembershipAudited(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	now := time.Unix(1_800_000_000, 0)
	h := NewSystem(db, secret).Webhook(func() time.Time { return now })
	send := func(kind, org, user string) {
		t.Helper()
		body := fmt.Sprintf(`{"type":"organizationMembership.%s","data":{"organization":{"id":%q},"public_user_data":{"user_id":%q}}}`, kind, org, user)
		req := httptest.NewRequest("POST", "/api/webhooks/clerk", strings.NewReader(body))
		req.Header.Set("svix-id", "msg_m")
		req.Header.Set("svix-timestamp", fmt.Sprint(now.Unix()))
		req.Header.Set("svix-signature", sign(key, "msg_m", now, body))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d", kind, rec.Code)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ('org_ma_pro', 'active', 'pro') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_ma_pro", "org_ma_free"} {
		send("created", org, "user_kim")
		send("deleted", org, "user_kim")
	}
	log := func(org string) string {
		rows, _ := db.Query(ctx, `SELECT action || ' ' || target_id || ' ' || actor_kind FROM audit_outbox WHERE org_id = $1 AND action LIKE 'member.%' ORDER BY queued`, org)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}
	if got := log("org_ma_pro"); got != "member.joined user_kim system, member.removed user_kim system" {
		t.Fatalf("pro: %s", got)
	}
	if got := log("org_ma_free"); got != "" {
		t.Fatalf("free: %s", got)
	}
}

// TL-690: the admin mirror follows Clerk's role, whoever changed it, and the org log says so: the
// system for a change made in Clerk, the granting admin for one made in TrackLin.es.
func TestMembershipRoleMirrored(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	now := time.Unix(1_800_000_000, 0)
	h := NewSystem(db, secret).Webhook(func() time.Time { return now })
	send := func(kind, user, role string) {
		t.Helper()
		body := fmt.Sprintf(`{"type":"organizationMembership.%s","data":{"organization":{"id":"org_rm"},"public_user_data":{"user_id":%q},"role":%q}}`, kind, user, role)
		req := httptest.NewRequest("POST", "/api/webhooks/clerk", strings.NewReader(body))
		req.Header.Set("svix-id", "msg_r")
		req.Header.Set("svix-timestamp", fmt.Sprint(now.Unix()))
		req.Header.Set("svix-signature", sign(key, "msg_r", now, body))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: %d", kind, rec.Code)
		}
	}
	admins := func() string {
		rows, _ := db.Query(ctx, `SELECT user_clerk_id FROM org_admins WHERE org_id = 'org_rm' ORDER BY 1`)
		got, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		return strings.Join(got, ",")
	}
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ('org_rm', 'active', 'pro') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	send("created", "user_sso", "org:admin")  // an SSO role mapping made them an admin on joining
	send("created", "user_new", "org:member") // a plain member
	send("updated", "user_new", "org:admin")  // promoted in Clerk's dashboard
	send("updated", "user_sso", "org:member") // demoted
	send("updated", "user_new", "org:admin")  // a repeat changes nothing
	if got := admins(); got != "user_new" {
		t.Fatalf("admins: %s", got)
	}
	// granted in TrackLin.es: the admin API writes the row as the granting admin
	if _, err := db.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_rm', 'user_mo', 'user_new')`); err != nil {
		t.Fatal(err)
	}
	send("updated", "user_mo", "org:admin") // Clerk's echo of that change: no second entry
	rows, _ := db.Query(ctx, `SELECT action || ' ' || target_id || ' ' || actor_kind || ' ' || coalesce(actor, '-') FROM audit_outbox WHERE org_id = 'org_rm' AND action LIKE 'admin.%' ORDER BY queued`)
	got, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	want := []string{"admin.granted user_sso system -", "admin.granted user_new system -", "admin.revoked user_sso system -", "admin.granted user_mo person user_new"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("org log:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

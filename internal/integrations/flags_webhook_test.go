package integrations

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

// Matches flags.gg's timestamp + "." + exact transmitted bytes contract.
func signFlags(secret, stamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyFlagsWebhook(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := []byte(`{"id":"event"}`)
	stamp := "1700000000"
	sig := signFlags("whsec_test", stamp, body)
	for _, tc := range []struct {
		name, secret, stamp, sig string
		body                     []byte
		want                     bool
	}{
		{"valid", "whsec_test", stamp, sig, body, true},
		{"tampered body", "whsec_test", stamp, sig, append(append([]byte{}, body...), ' '), false},
		{"wrong secret", "other", stamp, sig, body, false},
		{"missing signature", "whsec_test", stamp, "", body, false},
		{"invalid hex", "whsec_test", stamp, "v1=zz", body, false},
		{"wrong version", "whsec_test", stamp, "v2=" + strings.TrimPrefix(sig, "v1="), body, false},
		{"bad timestamp", "whsec_test", "no", sig, body, false},
		{"old", "whsec_test", "1699999699", signFlags("whsec_test", "1699999699", body), body, false},
		{"future", "whsec_test", "1700000301", signFlags("whsec_test", "1700000301", body), body, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyFlagsWebhook(tc.secret, tc.stamp, tc.sig, tc.body, now); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFlagsWebhook(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+strings.TrimPrefix(url, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	if sourceErr, databaseErr := m.Close(); sourceErr != nil || databaseErr != nil {
		t.Fatalf("close migrations: %v %v", sourceErr, databaseErr)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	org := "org_hook_" + uuid.NewString()
	otherOrg := org + "_other"
	box, err := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store := Store{DB: db, Box: box}
	if err := store.Connect(ctx, org, Flags, DefaultFlagsURL, "flg_test", "admin"); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO org_admins (org_id,user_clerk_id,granted_by) VALUES ($1,'admin','test')`, org)
	exec(`INSERT INTO org_admins (org_id,user_clerk_id,granted_by) VALUES ($1,'someone_else','test')`, otherOrg)
	var project, id string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id,name,key_prefix,flags_agent_id) VALUES ($1,'hooks','HOOK','agent-a') RETURNING id`, org).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id,type,title,needs_flag,position) VALUES ($1,'feature','webhook test',true,0) RETURNING id`, project).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tk, err := (tickets.Store{DB: db}).FlagTicketOf(ctx, org, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	f := (System{store: store, admins: organizations.Admins{DB: db}, now: func() time.Time { return now }}).Flags(FlagsAPI{})
	configure := func(user, activeOrg, body string) *httptest.ResponseRecorder {
		claims := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		claims.ActiveOrganizationID = activeOrg
		r := httptest.NewRequest("PUT", "/", strings.NewReader(body)).WithContext(clerk.ContextWithSessionClaims(ctx, claims))
		r.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		f.ConfigureWebhook(w, r)
		return w
	}
	const secret = "whsec_test_flags"
	if w := configure("admin", org, `{}`); w.Code != 400 {
		t.Fatalf("missing secret: %d", w.Code)
	}
	if w := configure("member", org, `{"secret":"`+secret+`"}`); w.Code != 403 {
		t.Fatalf("member: %d %s", w.Code, w.Body)
	}
	if w := configure("admin", otherOrg, `{"secret":"`+secret+`"}`); w.Code != 403 {
		t.Fatalf("other org admin: %d", w.Code)
	}
	if w := configure("admin", org, `{"secret":"`+secret+`"}`); w.Code != 200 || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("configure: %d %s", w.Code, w.Body)
	}
	var sealed []byte
	if err := db.QueryRow(ctx, `SELECT flags_webhook_secret FROM org_integrations WHERE org_id=$1`, org).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(secret)) {
		t.Fatal("secret stored as plaintext")
	}
	if _, err := box.OpenWithAAD(sealed, "flags-hook|other_org"); err == nil {
		t.Fatal("secret not bound to organization")
	}
	event := func(kind, ref, agent string, at time.Time, enabled bool) flagWebhookEvent {
		e := flagWebhookEvent{ID: uuid.NewString(), Type: kind, CreatedAt: at}
		e.Data.FlagID = "42"
		e.Data.Name = "hook"
		e.Data.Ticket = ref
		e.Data.AgentID = agent
		e.Data.EnvironmentID = "prod"
		e.Data.Enabled = enabled
		e.Data.Rollout = 100
		return e
	}
	send := func(e flagWebhookEvent, whichOrg, signingSecret string, stamp time.Time) int {
		t.Helper()
		body, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		r.SetPathValue("org", whichOrg)
		ts := strconv.FormatInt(stamp.Unix(), 10)
		r.Header.Set("X-Flags-Timestamp", ts)
		r.Header.Set("X-Flags-Signature", signFlags(signingSecret, ts, body))
		r.Header.Set("X-Flags-Event-ID", e.ID)
		w := httptest.NewRecorder()
		f.Webhook(w, r)
		return w.Code
	}
	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM ticket_flag_states WHERE ticket_id=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	e := event("flag.state_changed", tk.Key, "agent-a", now, true)
	for _, tc := range []struct {
		name, org, secret string
		stamp             time.Time
		want              int
	}{
		{"bad secret", org, "wrong", now, 401}, {"old signature", org, secret, now.Add(-6 * time.Minute), 401}, {"other organization", otherOrg, secret, now, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := send(e, tc.org, tc.secret, tc.stamp); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
	for _, unknown := range []flagWebhookEvent{event("flag.created", "HOOK-999999", "agent-a", now, true), event("flag.created", tk.Key, "other-agent", now, true), event("flag.created", "", "agent-a", now, true)} {
		if got := send(unknown, org, secret, now); got != 204 {
			t.Fatalf("unknown: %d", got)
		}
	}
	if count() != 0 {
		t.Fatal("unrelated event changed state")
	}
	for i := 0; i < 2; i++ {
		if got := send(e, org, secret, now); got != 204 {
			t.Fatalf("delivery: %d", got)
		}
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE target_id=$1 AND action='flag.state_changed'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("replay activity: %d %v", n, err)
	}
	if count() != 1 {
		t.Fatal("missing flag state")
	}
	// Old deliveries signed with a fresh attempt timestamp must not regress state.
	if got := send(event("flag.state_changed", tk.Key, "agent-a", now.Add(-time.Second), false), org, secret, now); got != 204 {
		t.Fatal(got)
	}
	var enabled bool
	if err := db.QueryRow(ctx, `SELECT enabled FROM ticket_flag_states WHERE ticket_id=$1`, id).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("stale event overwrote state: %v %v", enabled, err)
	}
	if got := send(event("flag.archived", tk.Key, "agent-a", now.Add(time.Second), false), org, secret, now); got != 204 {
		t.Fatal(got)
	}
	var archived bool
	if err := db.QueryRow(ctx, `SELECT archived FROM ticket_flag_states WHERE ticket_id=$1`, id).Scan(&archived); err != nil || !archived {
		t.Fatalf("archive: %v %v", archived, err)
	}
	// An activity failure must roll back the state and replay claim too, so the
	// same provider delivery can succeed on retry after a transient failure.
	exec(`CREATE FUNCTION fail_flag_activity() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.action = 'flag.created' THEN RAISE EXCEPTION 'temporary audit failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_flag_activity BEFORE INSERT ON audit_outbox FOR EACH ROW EXECUTE FUNCTION fail_flag_activity()`)
	retry := event("flag.created", tk.Key, "agent-a", now.Add(2*time.Second), true)
	if got := send(retry, org, secret, now); got != 500 {
		t.Fatalf("failed transaction: %d", got)
	}
	if err := db.QueryRow(ctx, `SELECT archived FROM ticket_flag_states WHERE ticket_id=$1`, id).Scan(&archived); err != nil || !archived {
		t.Fatalf("failed transaction changed state: %v %v", archived, err)
	}
	exec(`DROP TRIGGER fail_flag_activity ON audit_outbox; DROP FUNCTION fail_flag_activity()`)
	if got := send(retry, org, secret, now); got != 204 {
		t.Fatalf("retry: %d", got)
	}
	if err := db.QueryRow(ctx, `SELECT archived FROM ticket_flag_states WHERE ticket_id=$1`, id).Scan(&archived); err != nil || archived {
		t.Fatalf("retry did not restore state: %v %v", archived, err)
	}
	// Rotation immediately invalidates the previous signing secret.
	if w := configure("admin", org, `{"secret":"whsec_rotated"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if got := send(e, org, secret, now); got != 401 {
		t.Fatalf("rotated secret: %d", got)
	}
	if w := configure("admin", org, `{"secret":""}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if got := send(e, org, secret, now); got != 404 {
		t.Fatalf("disabled: %d", got)
	}
}

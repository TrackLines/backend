package clerkhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
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
		plain, _, err := keys.Create(ctx, owner, org, owner+"-"+org, apikeys.KindAI)
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
		_, _, _, _, err := keys.Owner(ctx, plain)
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

	// bob is deleted: his rows and keys go everywhere
	if code := send(`{"type":"user.deleted","data":{"id":"bob"}}`); code != http.StatusNoContent || live(bobA) || member("org_a", "bob") {
		t.Fatalf("user deleted: %d", code)
	}
	if !live(aliceB) || !live(carolC) {
		t.Fatal("user deletion touched other users")
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
		t.Fatal("org_c projects should be kept")
	}
	if !live(aliceB) || !member("org_b", "alice") {
		t.Fatal("org deletion touched another org")
	}

	// other event types are acknowledged and ignored
	if code := send(`{"type":"user.created","data":{"id":"dave"}}`); code != http.StatusNoContent {
		t.Fatalf("ignored type: %d", code)
	}
}

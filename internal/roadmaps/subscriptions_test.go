package roadmaps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/email"
)

// outbox is the Mailer in tests: it keeps what would have been sent.
type outbox struct {
	mu   sync.Mutex // publishing sends from another goroutine
	sent []email.Message
	fail error
}

func (o *outbox) On() bool { return true }
func (o *outbox) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sent)
}
func (o *outbox) Send(_ context.Context, msgs ...email.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		return o.fail
	}
	o.sent = append(o.sent, msgs...)
	return nil
}

func TestNormalEmail(t *testing.T) {
	for in, want := range map[string]string{
		" Reader@Example.Test ":         "reader@example.test",
		"Name <reader@example.test>":    "", // one plain address only
		"a@example.test,b@example.test": "",
		"reader@localhost":              "",
		"not an email":                  "",
		"":                              "",
	} {
		if got := normalEmail(in); got != want {
			t.Errorf("normalEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoadmapEmailSubscriptions(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+dsn[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM roadmaps WHERE owner_clerk_id IN ('rs1', 'rs2'); DELETE FROM projects WHERE owner_clerk_id IN ('rs1', 'rs2')`); err != nil {
		t.Fatal(err)
	}
	store := Store{DB: db}
	roadmap := func(org, title, visibility string) *Roadmap {
		var project string
		if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Subscriptions test') RETURNING id`, org).Scan(&project); err != nil {
			t.Fatal(err)
		}
		rm, err := store.Create(ctx, org, project, title, "", visibility)
		if err != nil {
			t.Fatal(err)
		}
		return rm
	}
	public, other, team := roadmap("rs1", "Launch <plan>", Public), roadmap("rs2", "Other org", Public), roadmap("rs1", "Team only", Team)

	mail, on := &outbox{}, true
	h := NewSystem(db).WithManagers(everyone{}).WithSubscriptions(Subscriptions{
		Mail: mail, On: func() bool { return on }, Site: "https://tracklin.test", API: "https://api.tracklin.test"})
	call := func(handler http.HandlerFunc, roadmapID, target, body, org string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if org != "" {
			req = req.WithContext(auth.WithAPIKeyUser(ctx, org, "subscriptions-test-agent"))
		}
		req.SetPathValue("id", roadmapID)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	subscribe := func(roadmapID, address string) *httptest.ResponseRecorder {
		return call(h.Subscribe, roadmapID, "/api/roadmaps/"+roadmapID+"/subscriptions", `{"email":"`+address+`"}`, "")
	}
	// tokenIn pulls the token out of the last email's link for an action (confirm or unsubscribe)
	tokenIn := func(action string) string {
		t.Helper()
		text := mail.sent[len(mail.sent)-1].Text
		_, rest, ok := strings.Cut(text, "/subscription?"+action+"=")
		if !ok {
			t.Fatalf("no %s link in %q", action, text)
		}
		token, _ := url.QueryUnescape(strings.Fields(rest)[0])
		return token
	}
	subscribers := func(roadmapID string) (total, verified int) {
		t.Helper()
		if err := db.QueryRow(ctx, `SELECT count(*), count(verified_at) FROM roadmap_subscriptions WHERE roadmap_id = $1`, roadmapID).Scan(&total, &verified); err != nil {
			t.Fatal(err)
		}
		return
	}

	// only public roadmaps, only a real address; nothing is saved or sent otherwise
	if rec := subscribe(team.ID, "reader@example.test"); rec.Code != http.StatusNotFound {
		t.Fatalf("team roadmap: %d %s", rec.Code, rec.Body)
	}
	if rec := subscribe(public.ID, "not an email"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "enter one email address") {
		t.Fatalf("bad address: %d %s", rec.Code, rec.Body)
	}
	if rec := subscribe("00000000-0000-0000-0000-000000000000", "reader@example.test"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown roadmap: %d", rec.Code)
	}
	if len(mail.sent) != 0 {
		t.Fatalf("nothing should have been sent: %+v", mail.sent)
	}

	// subscribing sends one confirmation; the address gets nothing else until it confirms
	if rec := subscribe(public.ID, "Reader@Example.test"); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("subscribe: %d %s", rec.Code, rec.Body)
	}
	if len(mail.sent) != 1 || mail.sent[0].To != "reader@example.test" || !strings.Contains(mail.sent[0].Text, "https://tracklin.test/r/"+public.ID+"/subscription?confirm=") {
		t.Fatalf("confirmation email: %+v", mail.sent)
	}
	if !strings.Contains(mail.sent[0].HTML, "Launch &lt;plan&gt;") {
		t.Errorf("the title should be escaped in HTML: %s", mail.sent[0].HTML)
	}
	if rec := subscribe(public.ID, "reader@example.test"); rec.Code != http.StatusAccepted || len(mail.sent) != 1 {
		t.Fatalf("a repeat within 10 minutes should send nothing more: %d, %d sent", rec.Code, len(mail.sent))
	}
	if err := h.notifySubscribers(ctx, public, &Update{Health: HealthOnTrack, Note: "early"}); err != nil || len(mail.sent) != 1 {
		t.Fatalf("an unconfirmed address must not get updates: %v, %d sent", err, len(mail.sent))
	}

	// after 10 minutes a repeat sends a new link, and the old one stops working
	old := tokenIn("confirm")
	if _, err := db.Exec(ctx, `UPDATE roadmap_subscriptions SET sent_at = now() - interval '11 minutes' WHERE roadmap_id = $1`, public.ID); err != nil {
		t.Fatal(err)
	}
	if rec := subscribe(public.ID, "reader@example.test"); rec.Code != http.StatusAccepted || len(mail.sent) != 2 {
		t.Fatalf("resend: %d, %d sent", rec.Code, len(mail.sent))
	}
	confirm := func(token string) *httptest.ResponseRecorder {
		return call(h.ConfirmSubscription, "", "/api/roadmap-subscriptions/confirm", `{"token":"`+token+`"}`, "")
	}
	if rec := confirm(old); rec.Code != http.StatusNotFound {
		t.Fatalf("replaced token: %d", rec.Code)
	}
	if rec := call(h.ConfirmSubscription, "", "/api/roadmap-subscriptions/confirm", `{}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no token: %d", rec.Code)
	}
	token := tokenIn("confirm")
	var confirmed struct {
		RoadmapID string `json:"roadmap_id"`
	}
	if rec := confirm(token); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &confirmed) != nil || confirmed.RoadmapID != public.ID {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	if rec := confirm(token); rec.Code != http.StatusOK { // a second click on the link is fine
		t.Fatalf("confirm twice: %d", rec.Code)
	}
	// already confirmed: a repeat answers the same and sends nothing, even much later
	if _, err := db.Exec(ctx, `UPDATE roadmap_subscriptions SET sent_at = now() - interval '1 day' WHERE roadmap_id = $1`, public.ID); err != nil {
		t.Fatal(err)
	}
	if rec := subscribe(public.ID, "reader@example.test"); rec.Code != http.StatusAccepted || len(mail.sent) != 2 {
		t.Fatalf("duplicate of a confirmed address: %d, %d sent", rec.Code, len(mail.sent))
	}
	if total, verified := subscribers(public.ID); total != 1 || verified != 1 {
		t.Fatalf("one confirmed row expected: %d, %d", total, verified)
	}

	// a second, unconfirmed reader and another org's roadmap: neither gets this roadmap's update
	subscribe(public.ID, "unconfirmed@example.test")
	subscribe(other.ID, "elsewhere@example.test")
	if rec := confirm(tokenIn("confirm")); rec.Code != http.StatusOK {
		t.Fatalf("confirm other org's reader: %d", rec.Code)
	}
	mail.sent = nil

	// publishing emails the confirmed subscriber, with their own unsubscribe link and one-click header
	publish := func(roadmapID, org string) *httptest.ResponseRecorder {
		return call(h.PublishUpdate, roadmapID, "/api/roadmaps/"+roadmapID+"/updates", `{"health":"at_risk","note":"Waiting on <review>"}`, org)
	}
	if rec := publish(public.ID, "rs2"); rec.Code != http.StatusNotFound { // another organization can't publish, so can't email
		t.Fatalf("other org publish: %d", rec.Code)
	}
	update := &Update{Health: HealthAtRisk, Note: "Waiting on <review>"}
	if err := h.notifySubscribers(ctx, public, update); err != nil {
		t.Fatal(err)
	}
	if len(mail.sent) != 1 || mail.sent[0].To != "reader@example.test" {
		t.Fatalf("update recipients: %+v", mail.sent)
	}
	got := mail.sent[0]
	if got.Subject != "Launch <plan>: At risk" || !strings.Contains(got.HTML, "Waiting on &lt;review&gt;") || !strings.Contains(got.Text, "https://tracklin.test/r/"+public.ID+"\n") {
		t.Errorf("update email: %+v", got)
	}
	leave := tokenIn("unsubscribe")
	if got.Headers["List-Unsubscribe"] != "<https://api.tracklin.test/api/roadmap-subscriptions/unsubscribe?token="+url.QueryEscape(leave)+">" ||
		got.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("one-click unsubscribe headers: %v", got.Headers)
	}
	for _, m := range mail.sent {
		if strings.Contains(m.Text+m.HTML, "unconfirmed@example.test") || strings.Contains(m.Text+m.HTML, "elsewhere@example.test") {
			t.Errorf("an email names another subscriber: %+v", m)
		}
	}

	// the publish endpoint itself sends it, after answering
	if rec := publish(public.ID, "rs1"); rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body)
	}
	for deadline := time.Now().Add(5 * time.Second); mail.count() < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("publishing an update didn't email the subscriber")
		}
	}

	// no endpoint gives addresses away: the roadmap and its updates never contain one
	for _, handler := range []http.HandlerFunc{h.Get, h.ListUpdates} {
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(auth.WithAPIKeyUser(ctx, "rs1", "subscriptions-test-agent"))
		req.SetPathValue("id", public.ID)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "example.test") {
			t.Errorf("owner read leaked a subscriber: %d %s", rec.Code, rec.Body)
		}
	}

	// the roadmap says whether readers can subscribe: public and switched on
	canSubscribe := func(rm *Roadmap, org string) bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if org != "" {
			req = req.WithContext(auth.WithAPIKeyUser(ctx, org, "subscriptions-test-agent"))
		}
		req.SetPathValue("id", rm.ID)
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		var out Roadmap
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("get: %d %s", rec.Code, rec.Body)
		}
		return out.CanSubscribe
	}
	if !canSubscribe(public, "") || canSubscribe(team, "rs1") {
		t.Error("can_subscribe should be true for a public roadmap only")
	}

	// a roadmap that stops being public stops emailing, without losing its subscribers
	mail.sent = nil
	private := *public
	private.Visibility = LoginOnly
	if err := h.notifySubscribers(ctx, &private, update); err != nil || len(mail.sent) != 0 {
		t.Fatalf("a roadmap that isn't public must not email: %v, %d sent", err, len(mail.sent))
	}

	// flag off: subscribing is 404, nothing is emailed, the roadmap says so; unsubscribing still works
	on = false
	if rec := subscribe(public.ID, "late@example.test"); rec.Code != http.StatusNotFound {
		t.Fatalf("flag off subscribe: %d", rec.Code)
	}
	if err := h.notifySubscribers(ctx, public, update); err != nil || len(mail.sent) != 0 {
		t.Fatalf("flag off must not email: %v, %d sent", err, len(mail.sent))
	}
	if canSubscribe(public, "") {
		t.Error("can_subscribe should be false with the flag off")
	}
	if rec := call(h.Unsubscribe, "", "/api/roadmap-subscriptions/unsubscribe?token="+url.QueryEscape(leave), "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("one-click unsubscribe: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.Unsubscribe, "", "/api/roadmap-subscriptions/unsubscribe", `{"token":"`+leave+`"}`, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe twice: %d", rec.Code) // a second click isn't an error
	}
	if rec := call(h.Unsubscribe, "", "/api/roadmap-subscriptions/unsubscribe", "", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unsubscribe without a token: %d", rec.Code)
	}
	on = true
	if err := h.notifySubscribers(ctx, public, update); err != nil || len(mail.sent) != 0 {
		t.Fatalf("an unsubscribed address must get nothing: %v, %+v", err, mail.sent)
	}
	if total, _ := subscribers(other.ID); total != 1 {
		t.Errorf("unsubscribing touched another roadmap's subscriber: %d", total)
	}

	// a provider failure on the confirmation is reported, not swallowed
	mail.fail = errors.New("resend is down")
	if rec := subscribe(public.ID, "unlucky@example.test"); rec.Code != http.StatusBadGateway {
		t.Fatalf("failed confirmation send: %d", rec.Code)
	}
	mail.fail = nil

	// unconfirmed addresses are dropped after a week; deleting a roadmap drops its subscribers
	if _, err := db.Exec(ctx, `UPDATE roadmap_subscriptions SET sent_at = now() - interval '8 days' WHERE email = 'unconfirmed@example.test'`); err != nil {
		t.Fatal(err)
	}
	subscribe(public.ID, "fresh@example.test")
	var stale int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM roadmap_subscriptions WHERE email = 'unconfirmed@example.test'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("stale unconfirmed address kept: %d, %v", stale, err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM roadmaps WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	if total, _ := subscribers(other.ID); total != 0 {
		t.Errorf("a deleted roadmap kept %d subscribers", total)
	}
}

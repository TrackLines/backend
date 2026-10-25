package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
)

func as(user, org string) context.Context {
	return clerk.ContextWithSessionClaims(context.Background(), &clerk.SessionClaims{
		RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
}

// call runs handler with the path values (id, delivery) and a JSON body.
func call(c context.Context, handler http.HandlerFunc, method, body string, path ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c)
	req.Header.Set("Accept", "application/json") // coded errors
	if len(path) > 0 {
		req.SetPathValue("id", path[0])
	}
	if len(path) > 1 {
		req.SetPathValue("delivery", path[1])
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func setupOrgs(t *testing.T) System {
	db, box := OpenDB(t)
	if _, err := db.Exec(context.Background(), `DELETE FROM webhook_endpoints WHERE org_id IN ('wh1', 'wh2', 'wh3');
		DELETE FROM org_billing WHERE org_id IN ('wh1', 'wh2', 'wh3'); DELETE FROM org_admins WHERE org_id IN ('wh1', 'wh2', 'wh3');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('wh1', 'user_admin', 'test'), ('wh2', 'user_other', 'test'), ('wh3', 'user_free', 'test');
		INSERT INTO org_billing (org_id, plan, stripe_status) VALUES ('wh1', 'pro', 'active'), ('wh2', 'enterprise', 'active')`); err != nil {
		t.Fatal(err)
	}
	return NewSystem(db, box, organizations.Admins{DB: db}, func() bool { return true })
}

const hook = `{"url":"https://93.184.215.14/hook","events":["ticket.created","ticket.moved"]}`

func TestEndpoints(t *testing.T) {
	h := setupOrgs(t)
	admin, other := as("user_admin", "wh1"), as("user_other", "wh2")

	// behind the flag; admins only (signed in or their API key); creating needs Pro or Enterprise
	off := h
	off.on = func() bool { return false }
	if rec := call(admin, off.List, "GET", ""); rec.Code != 404 {
		t.Fatalf("flag off: %d", rec.Code)
	}
	if rec := call(as("user_member", "wh1"), h.List, "GET", ""); rec.Code != 403 {
		t.Fatalf("member: %d", rec.Code)
	}
	if rec := call(auth.WithAPIKey(context.Background(), "user_member", "wh1", "", "personal"), h.Create, "POST", hook); rec.Code != 403 {
		t.Fatalf("member's key: %d", rec.Code)
	}
	if rec := call(auth.WithAPIKey(context.Background(), "user_admin", "wh1", "", "personal"), h.List, "GET", ""); rec.Code != 200 || rec.Body.String() != "[]\n" {
		t.Fatalf("admin's key: %d %s", rec.Code, rec.Body)
	}
	free := as("user_free", "wh3")
	if rec := call(free, h.List, "GET", ""); rec.Code != 200 {
		t.Fatalf("free list: %d", rec.Code)
	}
	if rec := call(free, h.Create, "POST", hook); rec.Code != 402 || !strings.Contains(rec.Body.String(), "webhooks_plan") {
		t.Fatalf("free create: %d %s", rec.Code, rec.Body)
	}

	// URL and event rules
	for _, u := range []string{"http://93.184.215.14/hook", "https://127.0.0.1/x", "https://10.1.2.3/x", "https://169.254.169.254/latest",
		"https://localhost/x", "https://user:pw@93.184.215.14/", "not a url", ""} {
		body := fmt.Sprintf(`{"url":%q,"events":["ticket.created"]}`, u)
		if rec := call(admin, h.Create, "POST", body); rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad_webhook_url") {
			t.Fatalf("url %q: %d %s", u, rec.Code, rec.Body)
		}
	}
	for _, ev := range []string{`[]`, `["ticket.exploded"]`, `["ping"]`, `null`} {
		body := `{"url":"https://93.184.215.14/hook","events":` + ev + `}`
		if rec := call(admin, h.Create, "POST", body); rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad_webhook_events") {
			t.Fatalf("events %s: %d %s", ev, rec.Code, rec.Body)
		}
	}

	// created: the secret once, its last 4 as the hint, never in the endpoint
	rec := call(admin, h.Create, "POST", hook)
	var created struct {
		Endpoint Endpoint `json:"endpoint"`
		Secret   string   `json:"secret"`
	}
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || !strings.HasPrefix(created.Secret, "whsec_") ||
		created.Endpoint.SecretHint != created.Secret[len(created.Secret)-4:] || !created.Endpoint.Enabled || created.Endpoint.DisabledReason != nil ||
		len(created.Endpoint.Events) != 2 || !strings.HasSuffix(created.Endpoint.CreatedAt, "Z") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := created.Endpoint.ID
	if rec := call(admin, h.List, "GET", ""); rec.Code != 200 || strings.Contains(rec.Body.String(), created.Secret) || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("list: %s", rec.Body)
	}

	// another org sees none of it
	for name, f := range map[string]http.HandlerFunc{"patch": h.Update, "delete": h.Delete, "rotate": h.Rotate, "test": h.Test, "deliveries": h.Deliveries} {
		if rec := call(other, f, "POST", `{"enabled":false}`, id); rec.Code != 404 {
			t.Fatalf("other org %s: %d", name, rec.Code)
		}
	}
	if rec := call(other, h.Redeliver, "POST", "", id, id); rec.Code != 404 {
		t.Fatalf("other org redeliver: %d", rec.Code)
	}

	// patch: off by hand, on again starts afresh; events and URL checked the same way
	if _, err := h.store.DB.Exec(context.Background(), `UPDATE webhook_endpoints SET consecutive_failures = 3 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var e Endpoint
	if rec := call(admin, h.Update, "PATCH", `{"enabled":false}`, id); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &e) != nil ||
		e.Enabled || e.DisabledReason == nil || *e.DisabledReason != "manual" {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if rec := call(admin, h.Update, "PATCH", `{"enabled":true,"events":["comment.created"]}`, id); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &e) != nil ||
		!e.Enabled || e.DisabledReason != nil || e.ConsecutiveFailures != 0 || len(e.Events) != 1 || e.URL != "https://93.184.215.14/hook" {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	if rec := call(admin, h.Update, "PATCH", `{"url":"https://10.0.0.1/"}`, id); rec.Code != 400 {
		t.Fatalf("patch url: %d", rec.Code)
	}
	if rec := call(admin, h.Update, "PATCH", `{"events":[]}`, id); rec.Code != 400 {
		t.Fatalf("patch events: %d", rec.Code)
	}

	// rotate: a new secret, shown once, with its hint
	var rotated struct{ Secret string }
	if rec := call(admin, h.Rotate, "POST", "", id); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rotated) != nil || rotated.Secret == created.Secret {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	list, _ := h.store.List(context.Background(), "wh1")
	if list[0].SecretHint != rotated.Secret[len(rotated.Secret)-4:] {
		t.Fatalf("hint after rotate: %+v", list[0])
	}
	var sealed []byte
	_ = h.store.DB.QueryRow(context.Background(), `SELECT secret FROM webhook_endpoints WHERE id = $1`, id).Scan(&sealed)
	if strings.Contains(string(sealed), rotated.Secret) {
		t.Fatal("secret stored in the clear")
	}

	// test: a ping is queued (202); free orgs can't
	if rec := call(admin, h.Test, "POST", "", id); rec.Code != 202 {
		t.Fatalf("test: %d", rec.Code)
	}
	var d struct {
		Deliveries []Delivery `json:"deliveries"`
		More       bool       `json:"more"`
	}
	if rec := call(admin, h.Deliveries, "GET", "", id); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil || len(d.Deliveries) != 1 ||
		d.Deliveries[0].Event != "ping" || d.Deliveries[0].State != "pending" || !strings.Contains(d.Deliveries[0].RequestBody, `"data":{"message":"ping"}`) {
		t.Fatalf("deliveries: %d %s", rec.Code, rec.Body)
	}

	// up to 10 per org
	for i := 1; i < MaxEndpoints; i++ {
		if rec := call(admin, h.Create, "POST", hook); rec.Code != 201 {
			t.Fatalf("endpoint %d: %d", i+1, rec.Code)
		}
	}
	if rec := call(admin, h.Create, "POST", hook); rec.Code != 409 || !strings.Contains(rec.Body.String(), "webhook_limit") {
		t.Fatalf("11th: %d %s", rec.Code, rec.Body)
	}

	// delete, with its deliveries
	if rec := call(admin, h.Delete, "DELETE", "", id); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := call(admin, h.Delete, "DELETE", "", id); rec.Code != 404 {
		t.Fatalf("delete again: %d", rec.Code)
	}
	if rec := call(admin, h.Deliveries, "GET", "", id); rec.Code != 404 {
		t.Fatalf("deliveries of deleted: %d", rec.Code)
	}
	if rec := call(admin, h.Update, "PATCH", `{}`, "not-a-uuid"); rec.Code != 404 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

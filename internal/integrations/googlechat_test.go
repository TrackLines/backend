package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
)

// chatSpaces records what's posted to each Google Chat webhook (by its space).
type chatSpaces struct{ got map[string][]string }

func (c *chatSpaces) RoundTrip(r *http.Request) (*http.Response, error) {
	var in struct {
		Text    string
		CardsV2 []struct {
			Card struct {
				Sections []struct {
					Widgets []struct {
						ButtonList struct {
							Buttons []struct {
								Text    string
								OnClick struct{ OpenLink struct{ URL string } }
							}
						}
					}
				}
			}
		}
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	got := in.Text
	for _, c := range in.CardsV2 { // a button reads as "[label](url)" here
		for _, s := range c.Card.Sections {
			for _, w := range s.Widgets {
				for _, b := range w.ButtonList.Buttons {
					got += " [" + b.Text + "](" + b.OnClick.OpenLink.URL + ")"
				}
			}
		}
	}
	space := strings.TrimPrefix(r.URL.Path, "/v1/spaces/")
	c.got[space] = append(c.got[space], got)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

// Google Chat (TL-288): a project's space and the org's get the kinds they chose, worded for a room;
// only Google's webhook URLs are taken; members can see it's set up, admins change it.
func TestGoogleChat(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'org_gc'; DELETE FROM chat_webhooks WHERE org_id = 'org_gc';
		DELETE FROM org_admins WHERE org_id = 'org_gc'; INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_gc', 'user_admin', 'test')`); err != nil {
		t.Fatal(err)
	}
	var shop, docs string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_gc', 'Shop', 'SHP') RETURNING id`).Scan(&shop)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_gc', 'Docs', 'DOC') RETURNING id`).Scan(&docs)
	ts := tickets.Store{DB: db}
	shopTicket, _ := ts.CreateBacklog(ctx, "org_gc", shop, "bug", "Checkout", "")
	docsTicket, _ := ts.CreateBacklog(ctx, "org_gc", docs, "task", "Write guide", "")

	box, _ := secrets.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	store := Store{DB: db, Box: box}
	spaces := &chatSpaces{got: map[string][]string{}}
	chat := &GoogleChat{Store: store, Site: "https://tracklin.es", Client: &http.Client{Transport: spaces}, Wait: true,
		People: func(context.Context, string) ([]notifications.Person, error) {
			return []notifications.Person{{ID: "user_ava", Names: []string{"Ava Smith"}}, {ID: "user_sam", Names: []string{"Sam Lee"}}}, nil
		}}
	h := System{store: store, admins: organizations.Admins{DB: db}, now: nil}.WithChat(chat)
	session := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = "org_gc"
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	call := func(hf http.HandlerFunc, c context.Context, method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(c)
		rec := httptest.NewRecorder()
		hf(rec, req)
		return rec
	}
	admin, member := session("user_admin"), session("user_member")
	hook := func(space string) string {
		return "https://chat.googleapis.com/v1/spaces/" + space + "/messages?key=k&token=t"
	}

	// only Google's webhook URLs; only admins set one
	for _, bad := range []string{"https://evil.example/v1/spaces/x/messages?key=k&token=t", "http://chat.googleapis.com/v1/spaces/x/messages?key=k&token=t",
		"https://chat.googleapis.com/v1/spaces/x/messages", "https://chat.googleapis.com/elsewhere?key=k&token=t"} {
		if rec := call(h.SetChat, admin, "PUT", "/", `{"url":"`+bad+`"}`); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := call(h.SetChat, member, "PUT", "/", `{"url":"`+hook("ORG")+`"}`); rec.Code != 403 {
		t.Fatalf("member set it: %d", rec.Code)
	}
	if rec := call(h.SetChat, admin, "PUT", "/", `{"url":"`+hook("ORG")+`","kinds":["created","assigned"]}`); rec.Code != 204 {
		t.Fatalf("org space: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.SetChat, admin, "PUT", "/", `{"project_id":"`+shop+`","url":"`+hook("SHOP")+`"}`); rec.Code != 204 { // default kinds
		t.Fatalf("project space: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.SetChat, admin, "PUT", "/", `{"kinds":["nope"]}`); rec.Code != 400 {
		t.Fatalf("unknown kind: %d", rec.Code)
	}
	// members see whether it's set up, never the URL
	rec := call(h.GetChat, member, "GET", "/?project_id="+shop, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"connected":true`) || strings.Contains(rec.Body.String(), "token=") {
		t.Fatalf("project space as a member: %d %s", rec.Code, rec.Body)
	}
	var stored []byte
	_ = db.QueryRow(ctx, `SELECT url FROM chat_webhooks WHERE org_id = 'org_gc' AND project_id IS NULL`).Scan(&stored)
	if bytes.Contains(stored, []byte("token=t")) {
		t.Fatal("webhook URL stored in the clear")
	}

	// a ticket made in Shop: both spaces; in Docs: only the org's; a kind a space didn't choose: not there
	notifications.Announce(ctx, chat, "org_gc", notifications.Event{Kind: "created", Text: "added SHP-1: Checkout to the backlog", Actor: "user_ava", TicketID: shopTicket.ID})
	notifications.Announce(ctx, chat, "org_gc", notifications.Event{Kind: "created", Text: "added DOC-1: Write guide to the backlog", Actor: "user_ava", TicketID: docsTicket.ID})
	_ = chat.Notify(ctx, "org_gc", []string{"user_ava"}, notifications.Event{Kind: "review", Text: "finished SHP-1: Checkout, and it needs your review", Actor: "user_sam", TicketID: shopTicket.ID})
	_ = chat.Notify(ctx, "org_gc", []string{"user_sam"}, notifications.Event{Kind: "assigned", Text: "assigned you SHP-1: Checkout", Actor: "user_ava", TicketID: shopTicket.ID})
	_ = chat.Notify(ctx, "org_gc", []string{"user_ava"}, notifications.Event{Kind: "mention", Text: "mentioned you in SHP-1", Actor: "user_sam", TicketID: shopTicket.ID})
	want := map[string][]string{
		"SHOP": {"*Ava Smith* added SHP-1: Checkout to the backlog [Open in TrackLin.es](https://tracklin.es/tickets/" + shopTicket.ID + ")",
			"*Sam Lee* finished SHP-1: Checkout, and it needs review [Open in TrackLin.es](https://tracklin.es/tickets/" + shopTicket.ID + ")"},
		"ORG": {"*Ava Smith* added SHP-1: Checkout to the backlog [Open in TrackLin.es](https://tracklin.es/tickets/" + shopTicket.ID + ")",
			"*Ava Smith* added DOC-1: Write guide to the backlog [Open in TrackLin.es](https://tracklin.es/tickets/" + docsTicket.ID + ")",
			"*Ava Smith* assigned SHP-1: Checkout to Sam Lee [Open in TrackLin.es](https://tracklin.es/tickets/" + shopTicket.ID + ")"},
	}
	for space, msgs := range want {
		if got := strings.Join(spaces.got[space+"/messages"], "\n---\n"); got != strings.Join(msgs, "\n---\n") {
			t.Errorf("%s got:\n%s\nwant:\n%s", space, got, strings.Join(msgs, "\n---\n"))
		}
	}

	// the test message goes to that space; removing it stops posts
	if rec := call(h.TestChat, admin, "POST", "/", `{"project_id":"`+shop+`"}`); rec.Code != 204 || len(spaces.got["SHOP/messages"]) != 3 {
		t.Fatalf("test message: %d %v", rec.Code, spaces.got["SHOP/messages"])
	}
	if rec := call(h.DeleteChat, admin, "DELETE", "/?project_id="+shop, ""); rec.Code != 204 {
		t.Fatalf("remove: %d", rec.Code)
	}
	notifications.Announce(ctx, chat, "org_gc", notifications.Event{Kind: "created", Text: "x", Actor: "user_ava", TicketID: shopTicket.ID})
	if len(spaces.got["SHOP/messages"]) != 3 {
		t.Fatal("a removed space still got posts")
	}
	// another org's project can't be set up from here
	if rec := call(h.SetChat, admin, "PUT", "/", `{"project_id":"00000000-0000-0000-0000-000000000000","url":"`+hook("X")+`"}`); rec.Code != 404 {
		t.Fatalf("someone else's project: %d", rec.Code)
	}
}

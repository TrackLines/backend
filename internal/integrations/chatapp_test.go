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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/notifications"
)

// fakeGoogle is Google for the Chat app: signing keys, OAuth tokens, and the Chat API (recording
// what's sent to each space; spaces in gone answer 404).
type fakeGoogle struct {
	mu    sync.Mutex
	sent  map[string][]string
	gone  map[string]bool
	dms   map[string]string // users/… → their chat with the app
	jwks  jose.JSONWebKeySet
	auths []string
	certs int // times the signing keys were fetched
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/certs":
		f.certs++
		_ = json.NewEncoder(w).Encode(f.jwks)
	case r.URL.Path == "/token":
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Form.Get("assertion") == "" {
			http.Error(w, "bad grant", 400)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"ya29.app","expires_in":3600}`))
	case r.URL.Path == "/chat/v1/spaces:findDirectMessage":
		space, ok := f.dms[r.URL.Query().Get("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"name":"` + space + `"}`))
	case strings.HasPrefix(r.URL.Path, "/chat/v1/spaces/") && strings.HasSuffix(r.URL.Path, "/messages"):
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		space := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/chat/v1/"), "/messages")
		if f.gone[space] {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Text    string
			CardsV2 []struct {
				Card struct {
					Sections []struct {
						Widgets []struct {
							ButtonList struct {
								Buttons []struct {
									Text    string
									OnClick struct {
										OpenLink struct{ URL string }
										Action   struct{ Parameters []struct{ Key, Value string } }
									}
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
						target := b.OnClick.OpenLink.URL
						for _, p := range b.OnClick.Action.Parameters { // an action button reads as "[label](do=…)"
							if p.Key == "do" {
								target = "do=" + p.Value
							}
						}
						got += " [" + b.Text + "](" + target + ")"
					}
				}
			}
		}
		f.sent[space] = append(f.sent[space], got)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGoogle) take(space string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.sent[space]
	delete(f.sent, space)
	return got
}

// Google Chat app (TL-289): only Google's signed requests are taken; a person links their account from
// the one-time link the app sends; then only their own notifications reach their chat with the app.
func TestChatApp(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM chat_dms; DELETE FROM chat_link_codes`); err != nil {
		t.Fatal(err)
	}

	googleKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	appKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	google := &fakeGoogle{sent: map[string][]string{}, gone: map[string]bool{}, dms: map[string]string{"users/home": "spaces/DMHOME"},
		jwks: jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{KeyID: "g1", Key: &googleKey.PublicKey, Algorithm: "RS256", Use: "sig"}}}}
	srv := httptest.NewServer(google)
	defer srv.Close()
	der, _ := x509.MarshalPKCS8PrivateKey(appKey)
	creds, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "tracklines-chat@p.iam.gserviceaccount.com", "private_key_id": "k1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": srv.URL + "/token"})
	app, err := NewChatApp(db, string(creds), "https://tracklin.es/", "https://api.tracklin.es/", "42")
	if err != nil {
		t.Fatal(err)
	}
	if app.Endpoint != "https://api.tracklin.es/api/webhooks/google-chat" {
		t.Fatalf("endpoint: %s", app.Endpoint)
	}
	now := time.Now()
	app.API, app.CertsURL, app.Client, app.Wait, app.now = srv.URL+"/chat", srv.URL+"/certs", srv.Client(), true, func() time.Time { return now }
	app.People = func(context.Context, string) ([]notifications.Person, error) {
		return []notifications.Person{{ID: "user_ava", Names: []string{"Ava Smith"}}, {ID: "user_sam", Names: []string{"Sam Lee"}}}, nil
	}

	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: googleKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "g1"))
	idToken := func(aud, email string) string {
		tok, _ := jwt.Signed(signer).Claims(map[string]any{"iss": "https://accounts.google.com", "aud": aud, "email": email, "email_verified": true,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}).Serialize()
		return tok
	}
	chatToken := idToken(app.Endpoint, "chat@system.gserviceaccount.com")
	event := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/webhooks/google-chat", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		app.Events(rec, req)
		return rec
	}
	dm := func(kind, user, space string) string {
		return `{"type":"` + kind + `","user":{"name":"` + user + `","displayName":"Ava","email":"ava@acme.test"},"space":{"name":"` + space + `","spaceType":"DIRECT_MESSAGE"}}`
	}
	codeIn := func(rec *httptest.ResponseRecorder) string {
		m := regexp.MustCompile(`/chat/link\?code=([A-Za-z0-9_-]+)`).FindStringSubmatch(rec.Body.String())
		if m == nil {
			t.Fatalf("no link in %s", rec.Body)
		}
		return m[1]
	}

	// only Google Chat's requests for our endpoint: no token, another audience, another signer
	for name, token := range map[string]string{
		"no token":       "",
		"other app":      idToken("https://elsewhere.example/hook", "chat@system.gserviceaccount.com"),
		"not chat":       idToken(app.Endpoint, "someone@gmail.com"),
		"other add-on":   idToken(app.Endpoint, "service-99@gcp-sa-gsuiteaddons.iam.gserviceaccount.com"),
		"forged":         strings.Replace(chatToken, chatToken[len(chatToken)-10:], "AAAAAAAAAA", 1),
		"not even a jwt": "hello",
	} {
		if rec := event(token, dm("MESSAGE", "users/ava", "spaces/DMAVA")); rec.Code != 401 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}

	// TL-427: made-up key ids fetch Google's keys again at most once a minute
	strange, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: googleKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "made-up"))
	strangeToken, _ := jwt.Signed(strange).Claims(map[string]any{"iss": "https://accounts.google.com", "aud": app.Endpoint, "email": "chat@system.gserviceaccount.com", "email_verified": true,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}).Serialize()
	fetched := func() int { google.mu.Lock(); defer google.mu.Unlock(); return google.certs }
	before := fetched()
	for range 3 {
		if rec := event(strangeToken, dm("MESSAGE", "users/ava", "spaces/DMAVA")); rec.Code != 401 {
			t.Fatalf("unknown key: %d", rec.Code)
		}
	}
	if got := fetched() - before; got != 0 {
		t.Fatalf("unknown kid fetched the keys %d more times within a minute", got)
	}
	now = now.Add(2 * time.Minute)
	event(strangeToken, dm("MESSAGE", "users/ava", "spaces/DMAVA"))
	event(strangeToken, dm("MESSAGE", "users/ava", "spaces/DMAVA"))
	if got := fetched() - before; got != 1 {
		t.Fatalf("after a minute: %d fetches, want 1", got)
	}

	// an unlinked person gets a one-time link; a room is told it's personal only
	rec := event(chatToken, dm("ADDED_TO_SPACE", "users/ava", "spaces/DMAVA"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Link your TrackLin.es account") || !strings.Contains(rec.Body.String(), "https://tracklin.es/chat/link?code=") {
		t.Fatalf("added: %d %s", rec.Code, rec.Body)
	}
	var top map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &top)
	if _, twice := top["text"]; twice { // the card only: not the words twice
		t.Fatalf("reply repeats itself: %s", rec.Body)
	}
	code := codeIn(rec)
	room := event(chatToken, `{"type":"ADDED_TO_SPACE","user":{"name":"users/ava"},"space":{"name":"spaces/ROOM","spaceType":"SPACE"}}`)
	if !strings.Contains(room.Body.String(), "only send personal messages") || strings.Contains(room.Body.String(), "code=") {
		t.Fatalf("room: %s", room.Body)
	}

	// help (TL-529, Marketplace review): "help", "@app help" and /help, in a DM or a space, in either
	// format, answer with how to get started; it makes no link code. Anything else points at help.
	isHelp := func(body string) bool {
		return strings.Contains(body, "TrackLin.es for Google Chat") && strings.Contains(body, "/help")
	}
	for name, body := range map[string]string{
		"dm help":         `{"type":"MESSAGE","user":{"name":"users/hal"},"space":{"name":"spaces/DMHAL","spaceType":"DIRECT_MESSAGE"},"message":{"text":"Help","argumentText":" Help "}}`,
		"space @app help": `{"type":"MESSAGE","user":{"name":"users/hal"},"space":{"name":"spaces/ROOM","spaceType":"SPACE"},"message":{"text":"@TrackLin.es help"}}`,
		"slash command":   `{"type":"MESSAGE","user":{"name":"users/hal"},"space":{"name":"spaces/ROOM","spaceType":"SPACE"},"message":{"text":"/help","slashCommand":{"commandId":"1"}}}`,
		"app command":     `{"type":"MESSAGE","user":{"name":"users/hal"},"space":{"name":"spaces/DMHAL","spaceType":"DIRECT_MESSAGE"},"appCommandMetadata":{"appCommandId":1,"appCommandType":"SLASH_COMMAND"}}`,
		"add-on message":  `{"chat":{"user":{"name":"users/hal"},"messagePayload":{"space":{"name":"spaces/DMHAL","spaceType":"DIRECT_MESSAGE"},"message":{"argumentText":"help"}}}}`,
		"add-on command":  `{"chat":{"user":{"name":"users/hal"},"appCommandPayload":{"space":{"name":"spaces/ROOM","spaceType":"SPACE"},"appCommandMetadata":{"appCommandId":1}}}}`,
	} {
		rec := event(chatToken, body)
		if rec.Code != 200 || !isHelp(rec.Body.String()) || strings.Contains(rec.Body.String(), "code=") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if strings.HasPrefix(name, "add-on") && !strings.Contains(rec.Body.String(), "createMessageAction") {
			t.Errorf("%s: not an add-on reply: %s", name, rec.Body)
		}
	}
	other := event(chatToken, `{"type":"MESSAGE","user":{"name":"users/hal"},"space":{"name":"spaces/DMHAL","spaceType":"DIRECT_MESSAGE"},"message":{"text":"is help coming?","slashCommand":{"commandId":"2"}}}`)
	if isHelp(other.Body.String()) || !strings.Contains(other.Body.String(), "Type help to see what I can do") {
		t.Fatalf("not help: %s", other.Body)
	}
	if !strings.Contains(room.Body.String(), "Type help") {
		t.Fatalf("room reply doesn't point at help: %s", room.Body)
	}

	// linking: signed in, once, before it expires; API keys can't
	session := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}})
	}
	call := func(h http.HandlerFunc, c context.Context, method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(c))
		return rec
	}
	if rec := call(app.Link, auth.WithAPIKeyUser(ctx, "user_ava", "claude"), "POST", `{"code":"`+code+`"}`); rec.Code != 403 {
		t.Fatalf("api key linked: %d", rec.Code)
	}
	// audit: the page first asks "link ava@acme.test?": looking doesn't use the code up; API keys and bad codes can't look
	look := func(c context.Context, code string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		app.LinkCode(rec, httptest.NewRequest("GET", "/?code="+code, nil).WithContext(c))
		return rec
	}
	if rec := look(session("user_ava"), code); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"google":"ava@acme.test"`) {
		t.Fatalf("which account: %d %s", rec.Code, rec.Body)
	}
	if rec := look(auth.WithAPIKeyUser(ctx, "user_ava", "claude"), code); rec.Code != 403 {
		t.Fatalf("api key looked: %d", rec.Code)
	}
	if rec := look(session("user_ava"), "nope"); rec.Code != 400 {
		t.Fatalf("bad code: %d", rec.Code)
	}
	if rec := call(app.Link, session("user_ava"), "POST", `{"code":"`+code+`"}`); rec.Code != 200 {
		t.Fatalf("link: %d %s", rec.Code, rec.Body)
	}
	if got := google.take("spaces/DMAVA"); len(got) != 1 || !strings.Contains(got[0], "You’re linked") {
		t.Fatalf("linked message: %v", got)
	}
	if rec := call(app.Link, session("user_eve"), "POST", `{"code":"`+code+`"}`); rec.Code != 400 { // used
		t.Fatalf("code reused: %d", rec.Code)
	}
	late := codeIn(event(chatToken, dm("MESSAGE", "users/bob", "spaces/DMBOB")))
	app.now = func() time.Time { return now.Add(31 * time.Minute) }
	if rec := call(app.Link, session("user_bob"), "POST", `{"code":"`+late+`"}`); rec.Code != 400 {
		t.Fatalf("expired code: %d", rec.Code)
	}
	app.now = func() time.Time { return now }
	if rec := call(app.Status, session("user_ava"), "GET", ""); !strings.Contains(rec.Body.String(), `"linked":true`) || !strings.Contains(rec.Body.String(), `"available":true`) {
		t.Fatalf("status: %s", rec.Body)
	}
	if rec := call(app.Status, session("user_bob"), "GET", ""); !strings.Contains(rec.Body.String(), `"linked":false`) {
		t.Fatalf("bob status: %s", rec.Body)
	}
	// a linked person messaging the app is told they're set
	if rec := event(chatToken, dm("MESSAGE", "users/ava", "spaces/DMAVA")); !strings.Contains(rec.Body.String(), "You’re linked") || strings.Contains(rec.Body.String(), "code=") {
		t.Fatalf("linked reply: %s", rec.Body)
	}

	// notifications: only the linked recipient, never the actor, with who did it and a link
	e := notifications.Event{Kind: "assigned", Text: "assigned you TL-12: Fix login", Actor: "user_sam", TicketID: "6a3ec0a8-9946-4ae6-85a5-4c34b95cd572"}
	if err := app.Notify(ctx, "org_x", []string{"user_ava", "user_bob", "user_ava", "claude"}, e); err != nil {
		t.Fatal(err)
	}
	got := google.take("spaces/DMAVA")
	if len(got) != 1 || got[0] != "*Sam Lee* assigned you TL-12: Fix login [Start](do=start) [Add comment](do=comment) [Make urgent](do=urgent) [Open in TrackLin.es](https://tracklin.es/tickets/6a3ec0a8-9946-4ae6-85a5-4c34b95cd572)" {
		t.Fatalf("assigned: %q", got)
	}
	google.mu.Lock()
	if a := google.auths[len(google.auths)-1]; a != "Bearer ya29.app" {
		t.Fatalf("sent with %q", a)
	}
	google.mu.Unlock()
	if err := app.Notify(ctx, "org_x", []string{"user_ava"}, notifications.Event{Kind: "mention", Text: "mentioned you", Actor: "user_ava"}); err != nil {
		t.Fatal(err)
	}
	if got := google.take("spaces/DMAVA"); len(got) != 0 {
		t.Fatalf("told about their own action: %v", got)
	}
	_ = app.Notify(ctx, "org_x", nil, notifications.Event{Kind: "created", Text: "created TL-13", Actor: "user_sam"}) // for spaces only
	if got := google.take("spaces/DMAVA"); len(got) != 0 {
		t.Fatalf("announcement in a DM: %v", got)
	}

	// the app's home tab (add-on format, no space): a card; their chat is found when first needed
	home := event(idToken(app.Endpoint, "service-42@gcp-sa-gsuiteaddons.iam.gserviceaccount.com"), `{"chat":{"user":{"name":"users/home"}}}`)
	if !strings.Contains(home.Body.String(), `"pushCard"`) {
		t.Fatalf("home: %s", home.Body)
	}
	if rec := call(app.Link, session("user_home"), "POST", `{"code":"`+codeIn(home)+`"}`); rec.Code != 200 {
		t.Fatalf("home link: %d", rec.Code)
	}
	if got := google.take("spaces/DMHOME"); len(got) != 1 {
		t.Fatalf("found chat: %v", got)
	}
	var space string
	_ = db.QueryRow(ctx, `SELECT space FROM chat_dms WHERE user_clerk_id = 'user_home'`).Scan(&space)
	if space != "spaces/DMHOME" {
		t.Fatalf("remembered %q", space)
	}
	// add-on message format replies wrapped
	if rec := event(idToken(app.Endpoint, "service-42@gcp-sa-gsuiteaddons.iam.gserviceaccount.com"),
		`{"chat":{"user":{"name":"users/home"},"messagePayload":{"space":{"name":"spaces/DMHOME","spaceType":"DIRECT_MESSAGE"}}}}`); !strings.Contains(rec.Body.String(), `"createMessageAction"`) {
		t.Fatalf("add-on reply: %s", rec.Body)
	}

	// the app removed from their chat: unlinked (by the event, or when sending fails)
	google.mu.Lock()
	google.gone["spaces/DMHOME"] = true
	google.mu.Unlock()
	_ = app.Notify(ctx, "org_x", []string{"user_home"}, e)
	if rec := call(app.Status, session("user_home"), "GET", ""); !strings.Contains(rec.Body.String(), `"linked":false`) {
		t.Fatalf("still linked after 404: %s", rec.Body)
	}
	if rec := event(chatToken, dm("REMOVED_FROM_SPACE", "users/ava", "spaces/DMAVA")); rec.Code != 200 {
		t.Fatalf("removed: %d", rec.Code)
	}
	if rec := call(app.Status, session("user_ava"), "GET", ""); !strings.Contains(rec.Body.String(), `"linked":false`) {
		t.Fatalf("still linked after removal: %s", rec.Body)
	}

	// unlinking yourself
	_, _ = db.Exec(ctx, `INSERT INTO chat_dms (user_clerk_id, google_user, space) VALUES ('user_sam', 'users/sam', 'spaces/DMSAM')`)
	if rec := call(app.Unlink, session("user_sam"), "DELETE", ""); rec.Code != 204 {
		t.Fatalf("unlink: %d", rec.Code)
	}
	_ = app.Notify(ctx, "org_x", []string{"user_sam"}, e)
	if got := google.take("spaces/DMSAM"); len(got) != 0 {
		t.Fatalf("messaged after unlinking: %v", got)
	}

	// the nightly purge removes link codes that expired unused, and only those
	_, _ = db.Exec(ctx, `DELETE FROM chat_link_codes`)
	if _, err := db.Exec(ctx, `INSERT INTO chat_link_codes (code_hash, google_user, expires_at) VALUES ('old', 'users/x', $1), ('fresh', 'users/y', $2)`,
		now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := PurgeLinkCodes(ctx, db, now); err != nil || n != 1 {
		t.Fatalf("purged %d (%v)", n, err)
	}
	var left string
	_ = db.QueryRow(ctx, `SELECT string_agg(code_hash, ',') FROM chat_link_codes`).Scan(&left)
	if left != "fresh" {
		t.Fatalf("left %q", left)
	}

	// a server without the app: says so, sends nothing
	var none *ChatApp
	if rec := call(none.Status, session("user_ava"), "GET", ""); rec.Body.String() != `{"available":false,"linked":false}`+"\n" {
		t.Fatalf("no app status: %s", rec.Body)
	}
	if rec := call(none.Events, ctx, "POST", "{}"); rec.Code != 503 {
		t.Fatalf("no app events: %d", rec.Code)
	}
	if err := none.Notify(ctx, "org_x", []string{"user_ava"}, e); err != nil {
		t.Fatal(err)
	}
	if a, err := NewChatApp(db, "", "https://tracklin.es", "https://api.tracklin.es", ""); a != nil || err != nil {
		t.Fatalf("no credentials: %v %v", a, err)
	}
	if _, err := NewChatApp(db, `{"client_email":"x"}`, "https://tracklin.es", "https://api.tracklin.es", ""); err == nil {
		t.Fatal("bad credentials accepted")
	}
}

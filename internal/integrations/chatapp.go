package integrations

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// Google Chat app (TL-289): personal notifications ("Ava assigned you TL-12") sent by the Tracklines
// Chat app into the private chat each person has with it. Spaces get team messages through webhooks
// (TL-288); this is the one-to-one side. A person opens a chat with the app, it sends them a one-time
// "Link your Tracklines account" link, and they open it signed in to Tracklines.

const (
	chatScope     = "https://www.googleapis.com/auth/chat.bot"
	chatIssuer    = "chat@system.gserviceaccount.com" // signs requests from Chat apps
	linkCodeTTL   = 30 * time.Minute
	chatAPIDomain = "https://chat.googleapis.com"
)

var ErrLinkCode = errors.New("this link has expired or was already used: message the TrackLin.es app in Google Chat for a new one")

// ChatApp sends personal notifications through the Chat app and answers Chat's requests. A nil
// *ChatApp is a server without the app set up: it notifies nobody and its endpoints say so.
type ChatApp struct {
	DB       *pgxpool.Pool
	Site     string // web app, for links
	Endpoint string // the app's HTTP endpoint URL: the audience Google signs requests for
	// ProjectNumber (optional): accept requests from this Cloud project's Workspace add-on too
	ProjectNumber string
	People        func(ctx context.Context, org string) ([]notifications.Person, error) // set after construction
	Client        *http.Client
	API           string // Chat API base; tests point it at a fake
	CertsURL      string // Google's signing keys
	Wait          bool   // tests: send before returning

	email, keyID, tokenURL string
	key                    *rsa.PrivateKey

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	keys     *jose.JSONWebKeySet
	keysAt   time.Time
	now      func() time.Time
}

// NewChatApp reads the app's service account key (the JSON Google downloads). No key: nil, no app.
// Google calls the backend directly (api, PUBLIC_API_URL): the web app's Clerk layer would refuse
// Google's token. site is the web app, for links.
func NewChatApp(db *pgxpool.Pool, credentials, site, api, projectNumber string) (*ChatApp, error) {
	if strings.TrimSpace(credentials) == "" {
		return nil, nil
	}
	sa, err := parseServiceAccount(credentials)
	if err != nil {
		return nil, fmt.Errorf("google chat: %w", err)
	}
	site = strings.TrimRight(site, "/")
	return &ChatApp{DB: db, Site: site, Endpoint: strings.TrimRight(api, "/") + "/api/webhooks/google-chat", ProjectNumber: projectNumber,
		Client: externalClient(10 * time.Second), API: chatAPIDomain, CertsURL: "https://www.googleapis.com/oauth2/v3/certs",
		email: sa.email, keyID: sa.keyID, tokenURL: sa.tokenURL, key: sa.key}, nil
}

func (c *ChatApp) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// --- requests from Google Chat ---

// verify checks the request is from Google Chat for this app: a Google-signed ID token for our
// endpoint, from Chat (or, with ProjectNumber, our project's add-on service account).
func (c *ChatApp) verify(ctx context.Context, r *http.Request) error {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errors.New("no bearer token")
	}
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return err
	}
	keys, err := c.signingKeys(ctx, tok)
	if err != nil {
		return err
	}
	var claims struct {
		jwt.Claims
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := tok.Claims(keys, &claims); err != nil {
		return err
	}
	if err := claims.ValidateWithLeeway(jwt.Expected{AnyAudience: jwt.Audience{c.Endpoint}, Time: c.clock()}, time.Minute); err != nil {
		return err
	}
	if claims.Issuer != "accounts.google.com" && claims.Issuer != "https://accounts.google.com" {
		return errors.New("not issued by Google")
	}
	addon := c.ProjectNumber != "" && claims.Email == "service-"+c.ProjectNumber+"@gcp-sa-gsuiteaddons.iam.gserviceaccount.com"
	if !claims.EmailVerified || (claims.Email != chatIssuer && !addon) {
		return fmt.Errorf("signed for %q, not Google Chat", claims.Email)
	}
	return nil
}

// signingKeys are Google's, cached for an hour and fetched again when a token names a key we don't have.
func (c *ChatApp) signingKeys(ctx context.Context, tok *jwt.JSONWebToken) (*jose.JSONWebKeySet, error) {
	kid := ""
	if len(tok.Headers) > 0 {
		kid = tok.Headers[0].KeyID
	}
	c.mu.Lock()
	keys, fresh := c.keys, c.clock().Sub(c.keysAt) < time.Hour
	c.mu.Unlock()
	if keys != nil && fresh && len(keys.Key(kid)) > 0 {
		return keys, nil
	}
	var set jose.JSONWebKeySet
	if err := c.call(ctx, http.MethodGet, c.CertsURL, "", nil, &set); err != nil {
		return nil, fmt.Errorf("google signing keys: %w", err)
	}
	c.mu.Lock()
	c.keys, c.keysAt = &set, c.clock()
	c.mu.Unlock()
	return &set, nil
}

type chatSpace struct {
	Name            string `json:"name"`
	Type            string `json:"type"`      // DM | ROOM (older field)
	SpaceType       string `json:"spaceType"` // DIRECT_MESSAGE | SPACE | GROUP_CHAT
	SingleUserBotDm bool   `json:"singleUserBotDm"`
}

func (s chatSpace) direct() bool {
	return s.Type == "DM" || s.SpaceType == "DIRECT_MESSAGE" || s.SingleUserBotDm
}

type chatUser struct {
	Name        string `json:"name"` // users/123…
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

// label: how the link page names this Google account (email, else name)
func (u chatUser) label() string {
	if u.Email != "" {
		return u.Email
	}
	return u.DisplayName
}

// chatRequest is either of Chat's formats: Chat app events ({type, space, user}) or, for an app
// built as a Workspace add-on, {chat: {user, messagePayload | addedToSpacePayload | …}}.
type chatRequest struct {
	Type  string    `json:"type"` // MESSAGE | ADDED_TO_SPACE | REMOVED_FROM_SPACE | APP_HOME | …
	Space chatSpace `json:"space"`
	User  chatUser  `json:"user"`
	Chat  *struct {
		User    chatUser `json:"user"`
		Message *struct {
			Space chatSpace `json:"space"`
		} `json:"messagePayload"`
		Added *struct {
			Space chatSpace `json:"space"`
		} `json:"addedToSpacePayload"`
		Removed *struct {
			Space chatSpace `json:"space"`
		} `json:"removedFromSpacePayload"`
	} `json:"chat"`
}

// event is what happened: "message", "added", "removed" or "home", by whom, where; addon says how to answer.
func (q chatRequest) event() (kind string, user chatUser, space chatSpace, addon bool) {
	if c := q.Chat; c != nil {
		switch {
		case c.Removed != nil:
			return "removed", c.User, c.Removed.Space, true
		case c.Added != nil:
			return "added", c.User, c.Added.Space, true
		case c.Message != nil:
			return "message", c.User, c.Message.Space, true
		}
		return "home", c.User, chatSpace{}, true
	}
	switch q.Type {
	case "REMOVED_FROM_SPACE":
		return "removed", q.User, q.Space, false
	case "ADDED_TO_SPACE":
		return "added", q.User, q.Space, false
	case "APP_HOME":
		return "home", q.User, q.Space, false
	}
	return "message", q.User, q.Space, false
}

// Events: POST /api/webhooks/google-chat, Chat's requests to the app. A person who isn't linked gets a
// one-time link to link their account; a linked one is told what they'll get and how to stop it.
func (c *ChatApp) Events(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		http.Error(w, "the Google Chat app isn't set up on this server", http.StatusServiceUnavailable)
		return
	}
	if err := c.verify(r.Context(), r); err != nil {
		logs.Warnf("google chat app: refused a request: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var q chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q); err != nil {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	kind, user, space, addon := q.event()
	ctx := r.Context()
	if kind == "removed" {
		if _, err := c.DB.Exec(ctx, `DELETE FROM chat_dms WHERE google_user = $1`, user.Name); err != nil {
			logs.Errorf("google chat app: unlink %s: %v", user.Name, err)
		}
		httpx.JSON(w, http.StatusOK, map[string]any{})
		return
	}
	if user.Name == "" {
		httpx.JSON(w, http.StatusOK, map[string]any{})
		return
	}
	var text, button, buttonURL string
	switch {
	case space.Name != "" && !space.direct():
		text = "I only send personal messages. Open a direct message with me to get yours. For a team space, add a webhook in TrackLin.es: Settings → Integrations → Google Chat."
	default:
		linked, err := c.linked(ctx, user.Name, space)
		if err != nil {
			logs.Errorf("google chat app: %v", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if linked {
			text = "You’re linked. I’ll message you here when you’re assigned a ticket, mentioned, or something needs your review. To stop, unlink it in TrackLin.es: Your settings, in your account menu."
			button, buttonURL = "Your settings", c.Site+"/account"
		} else {
			code, err := c.newLinkCode(ctx, user, space.Name)
			if err != nil {
				logs.Errorf("google chat app: link code: %v", err)
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			text = "Hi! Link your TrackLin.es account and I’ll message you here when you’re assigned a ticket, mentioned, or something needs your review. The link works once, for 30 minutes."
			button, buttonURL = "Link your TrackLin.es account", c.Site+"/chat/link?code="+url.QueryEscape(code)
		}
	}
	httpx.JSON(w, http.StatusOK, chatReply(kind, addon, text, button, buttonURL))
}

// chatReply answers in the request's format: a card pushed onto the app's home tab, or a message.
func chatReply(kind string, addon bool, text, button, buttonURL string) any {
	widgets := []any{map[string]any{"textParagraph": map[string]any{"text": text}}}
	if button != "" {
		widgets = append(widgets, map[string]any{"buttonList": map[string]any{"buttons": []any{
			map[string]any{"text": button, "onClick": map[string]any{"openLink": map[string]any{"url": buttonURL}}},
		}}})
	}
	card := map[string]any{"sections": []any{map[string]any{"widgets": widgets}}}
	if kind == "home" {
		return map[string]any{"action": map[string]any{"navigations": []any{map[string]any{"pushCard": card}}}}
	}
	// with a button, the card alone: replies don't format <url|label> links, so text would show it twice
	message := map[string]any{"text": text}
	if button != "" {
		message = map[string]any{"cardsV2": []any{map[string]any{"cardId": "tracklines", "card": card}}}
	}
	if addon {
		return map[string]any{"hostAppDataAction": map[string]any{"chatDataAction": map[string]any{"createMessageAction": map[string]any{"message": message}}}}
	}
	return message
}

// linked reports whether the Google user is linked, remembering their chat with the app if it's new.
func (c *ChatApp) linked(ctx context.Context, google string, space chatSpace) (bool, error) {
	tag, err := c.DB.Exec(ctx, `UPDATE chat_dms SET space = CASE WHEN $2 = '' THEN space ELSE $2 END WHERE google_user = $1`, google, space.Name)
	return tag.RowsAffected() > 0, err
}

func (c *ChatApp) newLinkCode(ctx context.Context, google chatUser, space string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	_, err := c.DB.Exec(ctx, `WITH gone AS (DELETE FROM chat_link_codes WHERE expires_at < now())
		INSERT INTO chat_link_codes (code_hash, google_user, space, expires_at, google_label) VALUES ($1, $2, $3, $4, $5)`,
		hashCode(code), google.Name, space, c.clock().Add(linkCodeTTL), google.label())
	return code, err
}

// PurgeLinkCodes deletes link codes that expired unused (the nightly purge job).
func PurgeLinkCodes(ctx context.Context, db *pgxpool.Pool, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM chat_link_codes WHERE expires_at < $1`, now)
	return tag.RowsAffected(), err
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// --- the person's side, in Tracklines ---

// Status: GET /api/me/google-chat: {"available": the app is set up, "linked": you've linked it}.
func (c *ChatApp) Status(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"available": c != nil, "linked": false}
	if c != nil {
		user, _ := auth.UserID(r.Context())
		var at time.Time
		err := c.DB.QueryRow(r.Context(), `SELECT linked_at FROM chat_dms WHERE user_clerk_id = $1`, user).Scan(&at)
		switch {
		case err == nil:
			out["linked"], out["linked_at"] = true, at
		case !errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// LinkCode: GET /api/me/google-chat/link?code=…: which Google account a code would link, without
// using it up, so the page asks before linking (audit: a link someone else sent used to link silently).
func (c *ChatApp) LinkCode(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		http.Error(w, "the Google Chat app isn't set up on this server", http.StatusServiceUnavailable)
		return
	}
	if _, ok := auth.UserID(r.Context()); !ok || auth.ViaAPIKey(r.Context()) {
		http.Error(w, "sign in to link Google Chat", http.StatusForbidden)
		return
	}
	var label string
	err := c.DB.QueryRow(r.Context(), `SELECT google_label FROM chat_link_codes WHERE code_hash = $1 AND expires_at > $2`,
		hashCode(r.URL.Query().Get("code")), c.clock()).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, ErrLinkCode.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		logs.Errorf("google chat app: link code: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"google": label})
}

// Link: POST /api/me/google-chat/link {"code"}: the signed-in person claims the Google user the code
// was made for. One Google account per person and the other way round: linking again replaces it.
func (c *ChatApp) Link(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		http.Error(w, "the Google Chat app isn't set up on this server", http.StatusServiceUnavailable)
		return
	}
	user, ok := auth.UserID(r.Context())
	if !ok || auth.ViaAPIKey(r.Context()) {
		http.Error(w, "sign in to link Google Chat", http.StatusForbidden)
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	var google, space string
	err := pgx.BeginFunc(r.Context(), c.DB, func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `DELETE FROM chat_link_codes WHERE code_hash = $1 AND expires_at > $2 RETURNING google_user, space`,
			hashCode(in.Code), c.clock()).Scan(&google, &space)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLinkCode
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM chat_dms WHERE user_clerk_id = $1 OR google_user = $2`, user, google); err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO chat_dms (user_clerk_id, google_user, space) VALUES ($1, $2, $3)`, user, google, space)
		return err
	})
	if errors.Is(err, ErrLinkCode) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		logs.Errorf("google chat app: link: %v", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	c.background(r.Context(), func(ctx context.Context) {
		if err := c.sendTo(ctx, user, google, space, "You’re linked to TrackLin.es. I’ll message you here when something needs you.", ""); err != nil {
			logs.Warnf("google chat app: linked message: %v", err)
		}
	})
	httpx.JSON(w, http.StatusOK, map[string]bool{"linked": true})
}

// Unlink: DELETE /api/me/google-chat: no more personal messages in Google Chat.
func (c *ChatApp) Unlink(w http.ResponseWriter, r *http.Request) {
	if c != nil {
		user, _ := auth.UserID(r.Context())
		if _, err := c.DB.Exec(r.Context(), `DELETE FROM chat_dms WHERE user_clerk_id = $1`, user); err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- sending ---

// Notify messages each linked recipient (people, once, never the actor) in the background.
func (c *ChatApp) Notify(ctx context.Context, org string, to []string, e notifications.Event) error {
	if c == nil {
		return nil
	}
	var users []string
	for _, u := range to {
		if strings.HasPrefix(u, "user_") && u != e.Actor && !slices.Contains(users, u) {
			users = append(users, u)
		}
	}
	if len(users) == 0 {
		return nil
	}
	c.background(ctx, func(ctx context.Context) {
		rows, err := c.DB.Query(ctx, `SELECT user_clerk_id, google_user, space FROM chat_dms WHERE user_clerk_id = ANY($1)`, users)
		if err != nil {
			logs.Warnf("google chat app: %s: %v", e.Kind, err)
			return
		}
		type dm struct{ user, google, space string }
		var dms []dm
		for rows.Next() {
			var d dm
			if rows.Scan(&d.user, &d.google, &d.space) == nil {
				dms = append(dms, d)
			}
		}
		rows.Close()
		if len(dms) == 0 {
			return
		}
		text := "*" + chatNamer(ctx, c.DB, c.People, org)(e.Actor) + "* " + e.Text
		link := eventLink(c.Site, e)
		for _, d := range dms {
			if err := c.sendTo(ctx, d.user, d.google, d.space, text, link); err != nil {
				logs.Warnf("google chat app: %s: %v", e.Kind, err)
			}
		}
	})
	return nil
}

func (c *ChatApp) background(ctx context.Context, f func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	if c.Wait {
		defer cancel()
		f(ctx)
		return
	}
	go func() {
		defer cancel()
		f(ctx)
	}()
}

// sendTo posts text into the person's chat with the app, with an "Open in Tracklines" button for link,
// finding that chat first if we don't know it. When the app was removed from it (403/404), the link is dropped: nothing more can arrive.
func (c *ChatApp) sendTo(ctx context.Context, user, google, space, text, link string) error {
	if space == "" {
		var found struct {
			Name string `json:"name"`
		}
		if err := c.call(ctx, http.MethodGet, c.API+"/v1/spaces:findDirectMessage?name="+url.QueryEscape(google), "", nil, &found); err != nil {
			return c.gone(ctx, user, err)
		}
		space = found.Name
		_, _ = c.DB.Exec(ctx, `UPDATE chat_dms SET space = $2 WHERE user_clerk_id = $1`, user, space)
	}
	if !strings.HasPrefix(space, "spaces/") || strings.ContainsAny(space, "?#") {
		return fmt.Errorf("odd space name %q", space)
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(chatMessage(text, link))
	return c.gone(ctx, user, c.call(ctx, http.MethodPost, c.API+"/v1/"+space+"/messages", token, body, nil))
}

// chatMessage is a Chat message: text, and an "Open in Tracklines" button when there's a link. Chat
// shows <url|label> links in app and webhook messages as typed, so the link is a button.
func chatMessage(text, link string) map[string]any {
	message := map[string]any{"text": text}
	if link != "" {
		message["cardsV2"] = []any{map[string]any{"cardId": "open", "card": map[string]any{"sections": []any{map[string]any{"widgets": []any{
			map[string]any{"buttonList": map[string]any{"buttons": []any{map[string]any{"text": "Open in TrackLin.es", "onClick": map[string]any{"openLink": map[string]any{"url": link}}}}}},
		}}}}}}
	}
	return message
}

type chatStatusError struct{ status int }

func (e chatStatusError) Error() string { return "google chat answered " + http.StatusText(e.status) }

func (c *ChatApp) gone(ctx context.Context, user string, err error) error {
	var se chatStatusError
	if errors.As(err, &se) && (se.status == http.StatusNotFound || se.status == http.StatusForbidden) {
		_, _ = c.DB.Exec(ctx, `DELETE FROM chat_dms WHERE user_clerk_id = $1`, user)
	}
	return err
}

// accessToken is the app's OAuth token (service account, chat.bot scope), reused until near expiry.
func (c *ChatApp) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.token != "" && now.Before(c.tokenExp.Add(-time.Minute)) {
		return c.token, nil
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: c.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", c.keyID))
	if err != nil {
		return "", err
	}
	assertion, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": c.email, "scope": chatScope, "aud": c.tokenURL, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}).Serialize()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := c.do(req, &out); err != nil {
		return "", fmt.Errorf("google token: %w", err)
	}
	c.token, c.tokenExp = out.AccessToken, now.Add(time.Duration(out.ExpiresIn)*time.Second)
	return c.token, nil
}

func (c *ChatApp) call(ctx context.Context, method, target, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if token == "" && strings.HasPrefix(target, c.API) {
		if token, err = c.accessToken(ctx); err != nil {
			return err
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	return c.do(req, out)
}

func (c *ChatApp) do(req *http.Request, out any) error {
	res, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return chatStatusError{res.StatusCode}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

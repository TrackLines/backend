package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/apikeys"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// Google Chat (TL-288): notifications posted into a Chat space through its incoming webhook. A
// project can have one (its team's space), and the org one for everything. Only team-shaped
// notifications are offered (no @mentions: those are personal), worded for a room.

// ChatKinds are the notifications a space can get; DefaultChatKinds are on unless changed.
var (
	ChatKinds        = []string{"created", "review", "approval", "over_capacity", "finished", "wont_do", "assigned", "sent_back", "retention"}
	DefaultChatKinds = []string{"created", "review", "approval", "over_capacity", "finished", "wont_do"}
)

var (
	ErrBadChatURL   = errors.New("that isn't a Google Chat webhook: copy it from the space's Apps & integrations → Webhooks (https://chat.googleapis.com/v1/spaces/…)")
	ErrBadChatKinds = errors.New("unknown notification kind")
)

// validChatURL: Google's webhook address and nothing else, so it can't be pointed anywhere else.
func validChatURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "chat.googleapis.com" && u.User == nil &&
		strings.HasPrefix(u.Path, "/v1/spaces/") && u.Query().Get("key") != "" && u.Query().Get("token") != ""
}

func chatAAD(org, project string) string { return "chat|" + org + "|" + project }

// ChatSpace is a space's settings as anyone sees them: never its URL.
type ChatSpace struct {
	Connected bool     `json:"connected"`
	Kinds     []string `json:"kinds"`
	UpdatedBy string   `json:"updated_by,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// chatSpace reads org's space for project ("" = the whole org).
func (s Store) chatSpace(ctx context.Context, org, project string) (ChatSpace, error) {
	c := ChatSpace{Kinds: DefaultChatKinds}
	err := s.DB.QueryRow(ctx, `SELECT kinds, updated_by, to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM chat_webhooks WHERE org_id = $1 AND coalesce(project_id::text, '') = $2`, org, project).Scan(&c.Kinds, &c.UpdatedBy, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	c.Connected = err == nil
	return c, err
}

// setChatSpace saves org's space for project: a new URL (sealed) or, with rawURL "", just its kinds.
func (s Store) setChatSpace(ctx context.Context, org, project, rawURL string, kinds []string, by string) error {
	var pid *string
	if project != "" {
		pid = &project
	}
	if rawURL == "" {
		tag, err := s.DB.Exec(ctx, `UPDATE chat_webhooks SET kinds = $3, updated_by = $4, updated_at = now()
			WHERE org_id = $1 AND coalesce(project_id::text, '') = $2`, org, project, kinds, by)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotConnected
		}
		return err
	}
	sealed, err := s.Box.SealWithAAD(rawURL, chatAAD(org, project))
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO chat_webhooks (org_id, project_id, url, kinds, updated_by) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, coalesce(project_id::text, '')) DO UPDATE SET url = $3, kinds = $4, updated_by = $5, updated_at = now()`,
		org, pid, sealed, kinds, by)
	return err
}

func (s Store) deleteChatSpace(ctx context.Context, org, project string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM chat_webhooks WHERE org_id = $1 AND coalesce(project_id::text, '') = $2`, org, project)
	return err
}

// chatTarget is a space to post to.
type chatTarget struct{ url string }

// chatTargets are the spaces that want kind for an event in project ("" when it has none): the
// project's own and the org's.
func (s Store) chatTargets(ctx context.Context, org, project, kind string) ([]chatTarget, error) {
	rows, err := s.DB.Query(ctx, `SELECT coalesce(project_id::text, ''), url FROM chat_webhooks
		WHERE org_id = $1 AND $3 = ANY(kinds) AND (project_id IS NULL OR project_id::text = $2)`, org, project, kind)
	if err != nil {
		return nil, err
	}
	type row struct {
		project string
		sealed  []byte
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.project, &x.sealed)
	})
	if err != nil {
		return nil, err
	}
	var out []chatTarget
	for _, x := range list {
		u, err := s.Box.OpenWithAAD(x.sealed, chatAAD(org, x.project))
		if err != nil {
			return nil, err
		}
		out = append(out, chatTarget{url: u})
	}
	return out, nil
}

// GoogleChat is the Notifier that posts to Chat spaces. Site is the web app's address, for links.
type GoogleChat struct {
	Store  Store
	Site   string
	People func(ctx context.Context, org string) ([]notifications.Person, error) // names people; set after construction
	Client *http.Client
	Wait   bool // tests: post before returning, not in the background
}

// Notify posts e to the spaces that want it, once each, in the background: a slow or broken
// space never holds up the inbox or push.
func (g *GoogleChat) Notify(ctx context.Context, org string, to []string, e notifications.Event) error {
	if g == nil || !slices.Contains(ChatKinds, e.Kind) {
		return nil
	}
	var project string
	switch {
	case e.TicketID != "":
		_ = g.Store.DB.QueryRow(ctx, `SELECT t.project_id::text FROM tickets t JOIN projects p ON p.id = t.project_id WHERE t.id::text = $1 AND p.owner_clerk_id = $2`, e.TicketID, org).Scan(&project)
	case e.BoardID != "":
		_ = g.Store.DB.QueryRow(ctx, `SELECT project_id::text FROM boards WHERE id::text = $1 AND owner_clerk_id = $2`, e.BoardID, org).Scan(&project)
	}
	targets, err := g.Store.chatTargets(ctx, org, project, e.Kind)
	if err != nil || len(targets) == 0 {
		return err
	}
	text := g.message(ctx, org, to, e)
	post := func() {
		for _, t := range targets {
			if err := g.post(context.WithoutCancel(ctx), t.url, text, g.link(e)); err != nil {
				logs.Warnf("google chat: %s: %v", e.Kind, err)
			}
		}
	}
	if g.Wait {
		post()
	} else {
		go post()
	}
	return nil
}

// message is e worded for a room: who did it (by name), what, and a link to open it.
// chatNamer names people and agents in Chat messages: "Ava Smith", "Claude", or "Someone".
func chatNamer(ctx context.Context, db *pgxpool.Pool, people func(context.Context, string) ([]notifications.Person, error), org string) func(string) string {
	names := map[string]string{}
	if people != nil {
		if list, err := people(ctx, org); err == nil {
			for _, p := range list {
				if len(p.Names) > 0 {
					names[p.ID] = p.Names[0]
				}
			}
		}
	}
	return func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		if strings.HasPrefix(id, "user_") {
			return "Someone"
		}
		var client, label *string // an agent key: "Claude", not claude-482193
		if db.QueryRow(ctx, `SELECT client, label FROM api_keys WHERE org_id = $1 AND name = $2 LIMIT 1`, org, id).Scan(&client, &label) == nil && client != nil {
			return apikeys.DisplayName(*client, deref(label))
		}
		return id
	}
}

func (g *GoogleChat) message(ctx context.Context, org string, to []string, e notifications.Event) string {
	name := chatNamer(ctx, g.Store.DB, g.People, org)
	text := e.Text
	switch e.Kind {
	case "review", "approval": // "…, and it needs your review" → "…, and it needs review"
		text = strings.Replace(text, "needs your ", "needs ", 1)
	case "assigned": // "assigned you TL-12: x" → "assigned TL-12: x to Sam"
		if len(to) > 0 {
			text = strings.TrimPrefix(text, "assigned you ")
			text = "assigned " + text + " to " + name(to[0])
		}
	}
	return "*" + name(e.Actor) + "* " + text
}

func (g *GoogleChat) link(e notifications.Event) string { return eventLink(g.Site, e) }

// eventLink is where a Chat message's "Open in Tracklines" goes: the ticket (by id, TL-276) or board.
func eventLink(site string, e notifications.Event) string {
	switch {
	case site == "":
		return ""
	case e.TicketID != "":
		return strings.TrimRight(site, "/") + "/tickets/" + e.TicketID
	case e.BoardID != "":
		return strings.TrimRight(site, "/") + "/boards/" + e.BoardID
	}
	return ""
}

func (g *GoogleChat) post(ctx context.Context, webhook, text, link string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(chatMessage(text, link))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	client := g.Client
	if client == nil {
		client = externalClient(10 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("google chat answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return nil
}

// HTTP: GET/PUT/DELETE /api/google-chat?project_id= (no project_id: the org's space); POST
// /api/google-chat/test. Any member sees whether it's set up; org admins change it.

func (h System) chatScope(w http.ResponseWriter, r *http.Request, org, project string) bool {
	if project != "" && !h.store.ownsProject(r.Context(), org, project) {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (h System) GetChat(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	project := r.URL.Query().Get("project_id")
	if org == "" || !h.chatScope(w, r, org, project) {
		if org == "" {
			http.Error(w, "sign in to an organization", http.StatusForbidden)
		}
		return
	}
	c, err := h.store.chatSpace(r.Context(), org, project)
	if err != nil {
		gitErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h System) SetChat(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		ProjectID string   `json:"project_id"`
		URL       string   `json:"url"`
		Kinds     []string `json:"kinds"`
	}
	if !httpx.Decode(w, r, &in) || !h.chatScope(w, r, org, in.ProjectID) {
		return
	}
	if in.URL = strings.TrimSpace(in.URL); in.URL != "" && !validChatURL(in.URL) {
		http.Error(w, ErrBadChatURL.Error(), http.StatusBadRequest)
		return
	}
	if in.Kinds == nil {
		in.Kinds = DefaultChatKinds
	}
	for _, k := range in.Kinds {
		if !slices.Contains(ChatKinds, k) {
			http.Error(w, ErrBadChatKinds.Error()+": "+k, http.StatusBadRequest)
			return
		}
	}
	if err := h.store.setChatSpace(r.Context(), org, in.ProjectID, in.URL, in.Kinds, user); err != nil {
		if errors.Is(err, ErrNotConnected) {
			http.Error(w, "paste the space's webhook URL first", http.StatusBadRequest)
			return
		}
		gitErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) DeleteChat(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok || !h.chatScope(w, r, org, r.URL.Query().Get("project_id")) {
		return
	}
	if err := h.store.deleteChatSpace(r.Context(), org, r.URL.Query().Get("project_id")); err != nil {
		gitErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TestChat posts a test message to the space, so an admin can see it's wired up: 204, or 502 with Google's answer.
func (h System) TestChat(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		ProjectID string `json:"project_id"`
	}
	if !httpx.Decode(w, r, &in) || !h.chatScope(w, r, org, in.ProjectID) {
		return
	}
	var sealed []byte
	if err := h.store.DB.QueryRow(r.Context(), `SELECT url FROM chat_webhooks WHERE org_id = $1 AND coalesce(project_id::text, '') = $2`, org, in.ProjectID).Scan(&sealed); err != nil {
		http.Error(w, "not connected", http.StatusConflict)
		return
	}
	webhook, err := h.store.Box.OpenWithAAD(sealed, chatAAD(org, in.ProjectID))
	if err != nil {
		gitErr(w, err)
		return
	}
	chat := h.chat
	if chat == nil {
		chat = &GoogleChat{Store: h.store}
	}
	if err := chat.post(r.Context(), webhook, "*Tracklines* is connected: notifications for this space will appear here. (Test sent by "+user+".)", ""); err != nil {
		http.Error(w, "Google Chat refused the test message: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

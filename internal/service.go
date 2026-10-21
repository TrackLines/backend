package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"

	"github.com/bugfixes/go-bugfixes/logs"
	bm "github.com/bugfixes/go-bugfixes/middleware"
	goFlags "github.com/flags-gg/go-flags"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"

	"github.com/tracklines/backend"
	"github.com/tracklines/backend/internal/apikeys"
	"github.com/tracklines/backend/internal/attachments"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/clerkcache"
	"github.com/tracklines/backend/internal/clerkhooks"
	"github.com/tracklines/backend/internal/columns"
	"github.com/tracklines/backend/internal/comments"
	"github.com/tracklines/backend/internal/config"
	"github.com/tracklines/backend/internal/enquiries"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/intake"
	"github.com/tracklines/backend/internal/integrations"
	"github.com/tracklines/backend/internal/invitations"
	"github.com/tracklines/backend/internal/legal"
	"github.com/tracklines/backend/internal/live"
	"github.com/tracklines/backend/internal/mcp"
	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/projects"
	"github.com/tracklines/backend/internal/ratelimit"
	"github.com/tracklines/backend/internal/roadmaps"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/shortlinks"
	"github.com/tracklines/backend/internal/sprints"
	"github.com/tracklines/backend/internal/teams"
	"github.com/tracklines/backend/internal/tickets"
	"github.com/tracklines/backend/internal/users"
)

// Service owns the HTTP lifecycle and route composition.
// cmd/server is limited to configuration, migrations and dependency construction.
type Service struct {
	Config  *config.Config
	DB      *pgxpool.Pool
	Valkey  valkey.Client   // optional (nil when unreachable at startup): caching only
	Flags   *goFlags.Client // s.Flags.Is("name").Enabled()
	Billing billing.Service
	Port    string
	// KeyMember overrides how API keys check their owner is still in the org (tests); nil asks Clerk
	KeyMember apikeys.Member

	calendarOnce sync.Once
	gcal         *integrations.GoogleCalendar // Google Calendar (TL-404); nil without a service account
}

func New(cfg *config.Config, db *pgxpool.Pool, vk valkey.Client, fl *goFlags.Client, bill billing.Service, port string) *Service {
	return &Service{Config: cfg, DB: db, Valkey: vk, Flags: fl, Billing: bill, Port: port}
}

// notifiers are the in-app inbox, Web Push (to people's devices, when the server has VAPID keys),
// Google Chat spaces (TL-288, when the server can seal their URLs), and all of them together: what
// everything that tells people something sends through.
func (s *Service) notifiers() (notifications.Store, *notifications.Push, *integrations.GoogleChat, *integrations.ChatApp, notifications.Notifier) {
	inbox := notifications.Store{DB: s.DB}
	pc := config.Get(s.Config)
	push := notifications.NewPush(s.DB, notifications.PushConfig{PublicKey: pc.VAPIDPublicKey, PrivateKey: pc.VAPIDPrivateKey, Subject: pc.VAPIDSubject})
	all := notifications.Multi{inbox}
	if push.Enabled() {
		all = append(all, push)
	}
	var chat *integrations.GoogleChat
	if box, err := secrets.New(pc.SecretKey); err == nil {
		chat = &integrations.GoogleChat{Store: integrations.Store{DB: s.DB, Box: box}, Site: siteURL(pc.AuthorizedParties())}
		all = append(all, chat)
	}
	// personal messages from the Chat app (TL-289): nil, and nothing sent, without its key
	app, err := integrations.NewChatApp(s.DB, pc.GoogleChatCredentials, siteURL(pc.AuthorizedParties()), pc.Git.PublicAPIURL, pc.GoogleChatProjectNumber)
	if err != nil {
		logs.Errorf("google chat app is off: %v", err)
	}
	if app != nil {
		all = append(all, app)
	}
	return inbox, push, chat, app, all
}

// calendarSync: Google Calendar's sync, with the one client the server has (nil without a service account).
func (s *Service) calendarSync(members clerkcache.Memberships) integrations.CalendarSync {
	s.calendarOnce.Do(func() {
		g, err := integrations.NewGoogleCalendar(s.DB, config.Get(s.Config).GoogleChatCredentials)
		if err != nil {
			logs.Errorf("google calendar is off: %v", err)
		}
		s.gcal = g
	})
	users := clerkcache.Users{Next: user.NewClient(&clerk.ClientConfig{}), VK: s.Valkey, TTL: time.Minute} // every verified email, not just the sign-in one
	return integrations.CalendarSync{DB: s.DB, Google: s.gcal, Members: members, Users: users}
}

// clerkHooks: Clerk's webhooks, which also give a new organization its member cap (TL-418).
func clerkHooks(db *pgxpool.Pool, secret string, uncapped []string) clerkhooks.System {
	h := clerkhooks.NewSystem(db, secret)
	h.ClerkOrgs = billing.Skip{OrgUpdater: organization.NewClient(&clerk.ClientConfig{}), Orgs: uncapped}
	return h
}

// flag is a flags.gg feature flag (cached by the client), off without flags.gg.
func (s *Service) flag(name string) func() bool {
	return func() bool { return s.Flags != nil && s.Flags.Is(name).Enabled() }
}

// siteURL is the web app's address (links in Google Chat): the first https origin Clerk allows.
func siteURL(parties []string) string {
	for _, p := range parties {
		if strings.HasPrefix(p, "https://") {
			return p
		}
	}
	return ""
}

func (s *Service) Start() error {
	// one shutdown signal for the server and background work, so auto-close stops before main
	// closes the database pool (it used to tick once more and log "closed pool" on every deploy)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// overdue sprints close themselves (manual close is POST /api/sprints/{id}/close); a next sprint
	// that starts over capacity tells the board's managers (TL-149)
	_, _, _, _, notify := s.notifiers()
	go sprints.Store{DB: s.DB, Notify: notify}.RunAutoClose(ctx, time.Minute)
	// every organization's Clerk member cap matches its plan: 20 on Free and Pro, unlimited on Enterprise (TL-418)
	go billing.SyncAllMemberCaps(ctx, s.DB, billing.Skip{OrgUpdater: organization.NewClient(&clerk.ClientConfig{}), Orgs: config.Get(s.Config).ClerkUncappedOrgs})
	// a new version of the Terms or Privacy policy on the web app: everyone's told once, in-app (TL-424)
	go legal.Announcer{DB: s.DB, Site: siteURL(config.Get(s.Config).AuthorizedParties())}.Run(ctx, time.Hour)
	// who's on holiday, from Google Calendar (TL-406): every connected org, hourly
	go s.calendarSync(clerkcache.Memberships{Next: organizationmembership.NewClient(&clerk.ClientConfig{}), VK: s.Valkey, TTL: time.Minute}).Run(ctx, time.Hour)
	// the bin and deleted organizations are purged, and deleted files removed, by Railway cron jobs
	// running this binary as `server purge` (00:00) and `server sweep-files` (00:30): see cmd/server (TL-256)
	if config.Get(s.Config).ClerkWebhookSecret == "" {
		// API keys still check membership on use, but removals, deletions and admin clean-up wait on it
		logs.Warnf("CLERK_WEBHOOK_SECRET is not set: Clerk webhooks are refused, so org removals aren't synced")
	}
	return s.serve(ctx, s.Handler())
}

// Handler is the whole API: routes plus the auth, logging and CORS middleware.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	// how many forwarding hops to trust for the client address, so rate limits can't be dodged with a
	// made-up X-Forwarded-For (TL finding: direct-origin spoofing). Set once, before serving.
	httpx.SetTrustedProxyHops(config.Get(s.Config).TrustedProxyHops)
	parties := config.Get(s.Config).AuthorizedParties()
	optional := auth.OptionalWithAuthorizedParties(parties...)
	required := auth.RequiredWith(optional)
	sessionRequired := auth.SessionRequiredWith(required)
	signedIn := func(h http.HandlerFunc) http.Handler { return required(h) }
	// public routes (no sign-in) are limited per client address; Valkey shares the count, memory without it
	limits := ratelimit.NewCounter(s.Valkey)
	public := func(name string, perMinute int, h http.HandlerFunc) http.Handler {
		return ratelimit.PerIP(limits, name, perMinute, time.Now)(h)
	}
	// Clerk member lists for pickers come from Valkey for a minute; checks that guard changes ask Clerk
	members := clerkcache.Memberships{Next: organizationmembership.NewClient(&clerk.ClientConfig{}), VK: s.Valkey, TTL: time.Minute}
	keyMember := s.KeyMember
	if keyMember == nil {
		keyMember = stillMember(members)
	}
	o := organizations.NewSystem(s.DB).CachingMembers(members)
	mux.Handle("GET /api/organizations/members", signedIn(o.Members))
	mux.Handle("GET /api/organizations/me", signedIn(o.Me))             // the caller's admin flag and the teams they lead
	mux.Handle("GET /api/organizations/admins", signedIn(o.ListAdmins)) // first admin is resolved from Clerk on demand
	mux.Handle("PUT /api/organizations/admins/{userID}", signedIn(o.GrantAdmin))
	mux.Handle("DELETE /api/organizations/admins/{userID}", signedIn(o.RevokeAdmin)) // never the last admin
	iv := invitations.NewSystem(s.DB, o.AdminModel())
	mux.Handle("GET /api/invitations", optional(http.HandlerFunc(iv.Mine))) // invitations addressed to the signed-in user's verified emails
	mux.Handle("GET /api/organizations/invitations", signedIn(iv.List))
	mux.Handle("POST /api/organizations/invitations", signedIn(iv.Create))
	mux.Handle("GET /api/organizations/invitations/{id}", signedIn(iv.Get))
	mux.Handle("DELETE /api/organizations/invitations/{id}", signedIn(iv.Revoke))
	mux.Handle("POST /api/organizations/invitations/{id}/accept", optional(http.HandlerFunc(iv.Accept)))
	tm := teams.NewSystem(s.DB, o.AdminModel())
	mux.Handle("GET /api/teams", signedIn(tm.List))
	mux.Handle("POST /api/teams", signedIn(tm.Create))
	mux.Handle("PATCH /api/teams/{id}", signedIn(tm.Update))
	mux.Handle("DELETE /api/teams/{id}", signedIn(tm.Delete))
	mux.Handle("GET /api/teams/{id}/members", signedIn(tm.Members))
	mux.Handle("PUT /api/teams/{id}/members/{userID}", signedIn(tm.AddMember))
	mux.Handle("DELETE /api/teams/{id}/members/{userID}", signedIn(tm.RemoveMember))
	mux.Handle("PUT /api/teams/{id}/leaders/{userID}", signedIn(tm.AddLeader)) // org admins only
	mux.Handle("DELETE /api/teams/{id}/leaders/{userID}", signedIn(tm.RemoveLeader))
	mux.Handle("PUT /api/boards/{id}/team", signedIn(tm.SetBoardTeam)) // the team whose leaders run the board
	mux.Handle("GET /api/projects/{projectID}/teams", signedIn(tm.ProjectTeams))
	mux.Handle("PUT /api/projects/{projectID}/teams/{teamID}", signedIn(tm.AddProjectTeam))
	mux.Handle("DELETE /api/projects/{projectID}/teams/{teamID}", signedIn(tm.RemoveProjectTeam))

	// Health
	mux.HandleFunc("GET /health", s.health)

	// Projects — a project holds a board per team plus its roadmaps; free tier = 1 project
	p := projects.NewSystem(s.DB, o.AdminModel())
	mux.Handle("GET /api/projects", signedIn(p.List))
	mux.Handle("POST /api/projects", signedIn(p.Create))
	mux.Handle("GET /api/projects/{id}", signedIn(p.Get))
	mux.Handle("PATCH /api/projects/{id}", signedIn(p.Update))
	mux.Handle("DELETE /api/projects/{id}", signedIn(p.Delete))

	// Boards
	b := boards.NewSystem(s.DB, o.AdminModel())
	mux.Handle("POST /api/projects/{id}/boards", signedIn(b.Create))  // optional template_id
	mux.Handle("GET /api/board-templates", signedIn(b.ListTemplates)) // built-ins + the org's own
	mux.Handle("POST /api/board-templates", signedIn(b.CreateTemplate))
	mux.Handle("DELETE /api/board-templates/{id}", signedIn(b.DeleteTemplate))
	mux.Handle("GET /api/boards/{id}", signedIn(b.Get))
	mux.Handle("GET /api/boards/{id}/flow", signedIn(b.Flow))
	mux.Handle("PATCH /api/boards/{id}", signedIn(b.Update))  // name and estimate_scale settings
	mux.Handle("DELETE /api/boards/{id}", signedIn(b.Delete)) // never-used boards only; used ones are archived
	mux.Handle("POST /api/boards/{id}/archive", signedIn(b.Archive))
	mux.Handle("POST /api/boards/{id}/restore", signedIn(b.Restore))
	mux.Handle("GET /api/projects/{id}/archived-boards", signedIn(b.Archived))

	// Columns
	c := columns.NewSystem(s.DB, o.AdminModel())
	mux.Handle("POST /api/boards/{boardID}/columns", signedIn(c.Create))
	mux.Handle("PUT /api/boards/{boardID}/columns/order", signedIn(c.Reorder))
	mux.Handle("PUT /api/boards/{boardID}/columns", signedIn(c.Layout)) // the edit page saves them all at once (TL-470)
	mux.Handle("PATCH /api/columns/{id}", signedIn(c.Update))           // name and/or wip_limit
	mux.Handle("DELETE /api/columns/{id}", signedIn(c.Delete))

	// Tickets
	// Notifications: an in-app inbox for now (external channels later implement the same Notifier)
	inbox, push, chat, chatApp, notify := s.notifiers()
	nf := notifications.NewSystem(inbox).WithPush(push)
	mux.Handle("GET /api/push", signedIn(nf.PushKey))
	// Google Chat app (TL-289): linking your own account (Chat's requests: see the end of Handler)
	mux.Handle("GET /api/me/google-chat", sessionRequired(http.HandlerFunc(chatApp.Status)))
	mux.Handle("GET /api/me/google-chat/link", sessionRequired(http.HandlerFunc(chatApp.LinkCode)))
	mux.Handle("POST /api/me/google-chat/link", sessionRequired(http.HandlerFunc(chatApp.Link)))
	mux.Handle("DELETE /api/me/google-chat", sessionRequired(http.HandlerFunc(chatApp.Unlink)))
	mux.Handle("POST /api/push/subscriptions", signedIn(nf.Subscribe))
	mux.Handle("DELETE /api/push/subscriptions", signedIn(nf.Unsubscribe))
	mux.Handle("GET /api/notifications", signedIn(nf.List))
	mux.Handle("GET /api/notifications/unread-count", signedIn(nf.UnreadCount))
	mux.Handle("POST /api/notifications/read-all", signedIn(nf.ReadAll))
	mux.Handle("POST /api/notifications/{id}/read", signedIn(nf.Read))
	mux.Handle("DELETE /api/notifications/read", signedIn(nf.ClearRead)) // clear everything already read
	mux.Handle("DELETE /api/notifications/{id}", signedIn(nf.Dismiss))
	t := tickets.NewSystem(s.DB).CachingMembers(members).WithNotifier(notify).
		CachingUsers(clerkcache.Users{Next: user.NewClient(&clerk.ClientConfig{}), VK: s.Valkey, TTL: time.Minute})
	push.People = t.People // pushes name who did it
	if chat != nil {
		chat.People = t.People // so do Google Chat posts
	}
	if chatApp != nil {
		chatApp.People = t.People // and the Chat app's personal messages
		// their buttons (TL-455) act as the linked person through these same routes, if still in the org
		chatApp.Act, chatApp.Member = actAs(mux), keyMember
	}
	mux.Handle("POST /api/columns/{id}/tickets", signedIn(t.Create))
	mux.Handle("GET /api/tickets/{id}", signedIn(t.Get))                // one ticket + where it lives (deep links)
	mux.Handle("GET /api/tickets/{id}/org", signedIn(t.Where))          // which of my orgs it's in, when it isn't in the active one
	mux.Handle("GET /api/tickets/{id}/history", signedIn(t.History))    // lifecycle: stages entered or skipped, when, by whom (TL-76)
	mux.Handle("GET /api/projects/{id}/epics", signedIn(t.Epics))       // epics with progress and size (TL-213)
	mux.Handle("GET /api/tickets/{id}/group", signedIn(t.GroupPreview)) // open sub-tickets that would move with it (TL-218)
	mux.Handle("GET /api/search", signedIn(t.Search))                   // full-text over titles, descriptions, comments; exact keys first
	mux.Handle("GET /api/ticket-keys/{key}", signedIn(t.GetByKey))
	mux.Handle("PUT /api/tickets/{id}/external", signedIn(t.SetExternal)) // integrations link what they made (API keys only)
	// Integrations: an org connects other tools (Bugfixes now); tickets they made show live details
	box, err := secrets.New(config.Get(s.Config).SecretKey)
	if err != nil {
		logs.Errorf("integrations disabled: %v", err)
	}
	gc := config.Get(s.Config).Git
	in := integrations.NewSystem(s.DB, box, s.Valkey, o.AdminModel(), integrations.Git{
		GitHubAppID: gc.GitHubAppID, GitHubAppSlug: gc.GitHubAppSlug, GitHubPrivateKey: gc.GitHubPrivateKey,
		GitHubClientID: gc.GitHubClientID, GitHubSecret: gc.GitHubClientSecret,
		BitbucketClientID:                                  gc.BitbucketClientID, BitbucketSecret: gc.BitbucketSecret, PublicAPI: gc.PublicAPIURL}).
		WithShortLinks(config.Get(s.Config).ShortLinkBase). // PRs can mention tickets by our short links (TL-283)
		WithChat(chat)                                      // Google Chat test messages (TL-288)
	// Google Chat (TL-288): a project's space, or the org's (no project_id); members see it, admins set it
	mux.Handle("GET /api/google-chat", signedIn(in.GetChat))
	mux.Handle("PUT /api/google-chat", signedIn(in.SetChat))
	mux.Handle("DELETE /api/google-chat", signedIn(in.DeleteChat))
	mux.Handle("POST /api/google-chat/test", signedIn(in.TestChat))
	// Google Calendar (TL-404): who's on holiday, read with the Chat app's service account once a
	// Workspace admin delegates to it. Before {provider}: more specific.
	calSync := s.calendarSync(members)
	cal := integrations.CalendarSystem{DB: s.DB, Admins: o.AdminModel(), Google: calSync.Google, Sync: &calSync, Users: calSync.Users}
	mux.Handle("GET /api/integrations/google-calendar", signedIn(cal.Get))
	mux.Handle("PUT /api/integrations/google-calendar", signedIn(cal.Connect)) // org admins; checks Google first
	mux.Handle("POST /api/integrations/google-calendar/test", signedIn(cal.Test))
	mux.Handle("POST /api/integrations/google-calendar/sync", signedIn(cal.SyncNow)) // admins; otherwise hourly
	mux.Handle("DELETE /api/integrations/google-calendar", signedIn(cal.Disconnect))
	mux.Handle("GET /api/integrations/{provider}", signedIn(in.Connection)) // connected or not; never the secret
	mux.Handle("PUT /api/integrations/{provider}", signedIn(in.Connect))    // org admins
	mux.Handle("DELETE /api/integrations/{provider}", signedIn(in.Disconnect))
	mux.Handle("GET /api/tickets/{id}/bugfixes", signedIn(in.TicketReport)) // the Bugfixes panel
	// GitHub (App) and Bitbucket (OAuth): org admins connect, then link repos to projects
	mux.Handle("GET /api/integrations/{provider}/connect", signedIn(in.ConnectURL)) // where to install/authorize
	mux.Handle("POST /api/integrations/{provider}/complete", signedIn(in.Complete)) // back from the provider
	mux.Handle("GET /api/integrations/{provider}/repos", signedIn(in.ListRepos))
	mux.Handle("GET /api/projects/{id}/repos", signedIn(in.ProjectRepos))
	mux.Handle("GET /api/tickets/{id}/prs", signedIn(in.TicketPRs)) // PRs that mention the ticket's key
	mux.Handle("POST /api/projects/{id}/repos", signedIn(in.LinkRepo))
	mux.Handle("DELETE /api/projects/{id}/repos", signedIn(in.UnlinkRepo)) // ?provider=&full_name=
	// webhooks: generous per-address limits (providers deliver from a few shared addresses)
	mux.Handle("POST /api/webhooks/github", public("webhook-github", 600, in.GitHubWebhook(gc.GitHubWebhookSecret)))
	mux.Handle("POST /api/webhooks/bitbucket/{link}", public("webhook-bitbucket", 600, in.BitbucketWebhook)) // one hook and secret per linked repo (TL-165)
	mux.Handle("PATCH /api/tickets/{id}", signedIn(t.Update))
	mux.Handle("POST /api/tickets/{id}/claim", signedIn(t.Claim))
	mux.Handle("POST /api/tickets/{id}/release", signedIn(t.Release))
	mux.Handle("PUT /api/tickets/{id}/assignee", signedIn(t.Assign))  // owner assigns to themself or an AI key
	mux.Handle("PUT /api/tickets/{id}/labels", signedIn(t.SetLabels)) // free-form labels; [] clears
	mux.Handle("GET /api/assignees", signedIn(t.Assignees))
	mux.Handle("DELETE /api/tickets/{id}", signedIn(t.Delete))
	// moving an unassigned ticket assigns it to the mover, except a board's manager during its standup (TL-266)
	mux.Handle("POST /api/tickets/{id}/move", signedIn(t.AssigningOnMove(o.AdminModel().IsBoardManager, live.New(s.Valkey, boards.Store{DB: s.DB}).StandupRunning).Move)) // also pulls a ticket out of the backlog
	mux.Handle("POST /api/tickets/{id}/review", signedIn(t.ReviewTicket))
	mux.Handle("POST /api/tickets/{id}/backlog", signedIn(t.ToBacklog))
	mux.Handle("POST /api/tickets/{id}/resolve", signedIn(t.Resolve)) // close a backlog ticket in place; completes finished parents
	mux.Handle("POST /api/tickets/{id}/duplicate", signedIn(t.MarkDuplicate))
	mux.Handle("PUT /api/tickets/{id}/parent", signedIn(t.SetParent))        // sub-tickets; null detaches
	mux.Handle("PUT /api/tickets/{id}/blocked-by", signedIn(t.SetBlockedBy)) // dependencies; claim is refused while blocked

	// Comments — a ticket's conversation, separate from its description; replies via parent_comment_id
	cm := comments.NewSystem(s.DB).WithMentions(notify, t.People)
	mux.Handle("GET /api/tickets/{id}/comments", signedIn(cm.List))
	mux.Handle("POST /api/tickets/{id}/comments", signedIn(cm.Create))
	mux.Handle("GET /api/mentions", signedIn(cm.Mentions)) // tickets that @mention the caller, newest first

	// Short links for "Copy link": our own site's links only, through 1tn, each made once
	sl := shortlinks.System{DB: s.DB, API: config.Get(s.Config).ShortLinkAPI, Base: config.Get(s.Config).ShortLinkBase, Sites: config.Get(s.Config).AuthorizedParties()}
	mux.Handle("POST /api/short-links", signedIn(sl.Create))
	mux.Handle("GET /api/mentions/page", signedIn(cm.PageMentions)) // the same, a page at a time with a total; hide_done=true

	// Attachments — files live on UploadThing (browser uploads via the Next.js route); we keep the records
	at := attachments.NewSystem(s.DB)
	at.IsAdmin = o.AdminModel().IsAdmin
	mux.Handle("GET /api/tickets/{id}/attachments", signedIn(at.List))
	mux.Handle("POST /api/tickets/{id}/attachments", signedIn(at.Create))
	mux.Handle("DELETE /api/attachments/{id}", signedIn(at.Delete))
	mux.Handle("POST /api/attachments/{id}/keep", signedIn(at.Keep))     // a year-old file: keep it another year (admins, uploader)
	mux.Handle("GET /api/attachments/retention", signedIn(at.Retention)) // files due for clean-up and already cleaned up (admins)

	// Backlog — project tickets not on any board/sprint yet (e.g. triaged bugs)
	mux.Handle("GET /api/projects/{id}/backlog", signedIn(t.Backlog))
	mux.Handle("GET /api/projects/{id}/open-tickets", signedIn(t.OpenTickets)) // not done, boards + backlog, in pick order (MCP list_open_tickets)
	mux.Handle("GET /api/projects/{id}/backlog/page", signedIn(t.PageBacklog)) // paged + filtered, for the project page
	mux.Handle("POST /api/projects/{id}/backlog", signedIn(t.CreateBacklog))
	mux.Handle("GET /api/projects/{id}/labels", signedIn(t.ProjectLabels)) // labels in use, for suggestions

	// Sprints — per team board; closing by hand just closes (the next waits for start); running out rolls into the next
	sp := sprints.NewSystem(s.DB, o.AdminModel()).WithLive(s.Valkey).WithNotifier(notify)
	lv := live.New(s.Valkey, boards.Store{DB: s.DB})
	lv.IsAdmin = o.AdminModel().IsAdmin
	mux.Handle("GET /api/live/sessions", signedIn(lv.RunningHandler)) // every running standup and refinement in the org (TL-466)
	mux.Handle("POST /api/boards/{id}/live/standup", signedIn(lv.Start))
	mux.Handle("GET /api/boards/{id}/live/standup", signedIn(lv.Get))
	mux.Handle("POST /api/boards/{id}/live/standup/action", signedIn(lv.Act))
	mux.Handle("GET /api/boards/{id}/live", signedIn(lv.Stream))
	// hosted standup (TL-225): the starter hosts; others join and see the board and ticket the host has open
	mux.Handle("GET /api/boards/{id}/live/standup/session", signedIn(lv.StandupSession))
	mux.Handle("POST /api/boards/{id}/live/standup/join", signedIn(lv.StandupJoin)) // also the 30s heartbeat
	mux.Handle("POST /api/boards/{id}/live/standup/leave", signedIn(lv.StandupLeave))
	mux.Handle("PUT /api/boards/{id}/live/standup/focus", signedIn(lv.StandupFocus)) // host only
	mux.Handle("PUT /api/boards/{id}/live/standup/host", signedIn(lv.StandupHost))   // hand over / take over
	// hosted refinement (TL-223): one per project; others join and follow what the host has open
	mux.Handle("GET /api/projects/{id}/live", signedIn(lv.ProjectStream))
	mux.Handle("POST /api/projects/{id}/live/refinement", signedIn(lv.RefinementStart))
	mux.Handle("GET /api/projects/{id}/live/refinement", signedIn(lv.RefinementGet))
	mux.Handle("DELETE /api/projects/{id}/live/refinement", signedIn(lv.RefinementEnd))     // host only
	mux.Handle("POST /api/projects/{id}/live/refinement/join", signedIn(lv.RefinementJoin)) // also the 30s heartbeat
	mux.Handle("POST /api/projects/{id}/live/refinement/leave", signedIn(lv.RefinementLeave))
	mux.Handle("PUT /api/projects/{id}/live/refinement/focus", signedIn(lv.RefinementFocus)) // host only
	mux.Handle("PUT /api/projects/{id}/live/refinement/host", signedIn(lv.RefinementHost))   // hand over / take over
	mux.Handle("GET /api/boards/{id}/sprints", signedIn(sp.List))
	mux.Handle("POST /api/boards/{id}/sprints", signedIn(sp.Start))
	mux.Handle("GET /api/boards/{id}/velocity", signedIn(sp.Velocity))               // per closed sprint + open sprint burn
	mux.Handle("GET /api/boards/{id}/reports/sprints", signedIn(sp.BoardReport))     // sprint reports (TL-397); ?from=&to=, ?format=md
	mux.Handle("GET /api/projects/{id}/reports/sprints", signedIn(sp.ProjectReport)) // a project's sprint boards together (TL-398)
	mux.Handle("GET /api/org/reports/sprints", signedIn(sp.OrgReport))               // every project; org admins only
	mux.Handle("GET /api/sprints/{id}", signedIn(sp.Get))                            // read-only: burn data and tickets
	mux.Handle("PATCH /api/sprints/{id}", signedIn(sp.Update))                       // change an open sprint's length and/or goal
	mux.Handle("POST /api/sprints/{id}/close", signedIn(sp.Close))
	// Refinement: up to 2 planned sprints per board; backlog tickets are planned and sized into them
	mux.Handle("GET /api/boards/{id}/planned-sprints", signedIn(sp.ListPlanned)) // with tickets and capacity (velocity + 10%)
	mux.Handle("POST /api/boards/{id}/planned-sprints", signedIn(sp.Plan))
	mux.Handle("PATCH /api/planned-sprints/{id}", signedIn(sp.UpdatePlanned)) // length and/or goal
	mux.Handle("DELETE /api/planned-sprints/{id}", signedIn(sp.Unplan))
	mux.Handle("POST /api/planned-sprints/{id}/approve", signedIn(sp.Approve)) // accept the total over capacity
	mux.Handle("PUT /api/tickets/{id}/planned-sprint", signedIn(sp.PlanTicket))

	// Request intake: a project's unlisted public form (no account) and the team's triage inbox
	ik := intake.NewSystem(s.DB).WithNotifier(notify).WithAdmins(o.AdminModel()) // a promoted request is announced to the project's spaces (TL-288)
	// the API's own description, public, built into the binary so it matches what's running (TL-484):
	// YAML for tools, JSON for the website's API reference
	specJSON, err := backend.OpenAPIJSON()
	if err != nil {
		logs.Warnf("openapi: %v", err)
	}
	mux.Handle("GET /api/openapi.yaml", public("openapi", 60, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(backend.OpenAPIYAML)
	}))
	mux.Handle("GET /api/openapi.json", public("openapi", 60, func(w http.ResponseWriter, _ *http.Request) {
		if specJSON == nil {
			http.Error(w, "the API description is unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(specJSON)
	}))
	mux.Handle("GET /api/intake/{token}", public("intake-form", 60, ik.Form))
	mux.Handle("POST /api/intake/{token}", public("intake-submit", 10, ik.Submit)) // on top of the per-sender hourly cap
	mux.Handle("GET /api/projects/{id}/intake", signedIn(ik.Settings))
	mux.Handle("PUT /api/projects/{id}/intake", signedIn(ik.SetOpen))
	mux.Handle("GET /api/projects/{id}/intake/submissions", signedIn(ik.List))
	mux.Handle("POST /api/intake-submissions/{id}/promote", signedIn(ik.Promote))
	mux.Handle("POST /api/intake-submissions/{id}/dismiss", signedIn(ik.Dismiss))

	// Roadmaps — GET by id is open: public ones are readable by anyone with the link
	// writes: org admins and the project's team leaders (TL-413); updates name who posted them
	r := roadmaps.NewSystem(s.DB).WithManagers(o.AdminModel()).WithPeople(t.People)
	mux.Handle("GET /api/roadmaps/{id}", public("roadmap", 120, r.Get))
	mux.Handle("GET /api/roadmaps", signedIn(r.List))
	mux.Handle("POST /api/projects/{id}/roadmaps", signedIn(r.Create))
	mux.Handle("PUT /api/roadmaps/{id}", signedIn(r.Update))
	mux.Handle("DELETE /api/roadmaps/{id}", signedIn(r.Delete))
	mux.Handle("PUT /api/roadmaps/{id}/items", signedIn(r.ReplaceItems))
	mux.Handle("PUT /api/roadmaps/{id}/items/{item}/tickets", signedIn(r.SetItemTickets))       // item progress = linked tickets done
	mux.Handle("GET /api/roadmaps/{id}/updates", public("roadmap-updates", 120, r.ListUpdates)) // public according to roadmap visibility
	mux.Handle("POST /api/roadmaps/{id}/updates", signedIn(r.PublishUpdate))

	// API keys — AI agent or server/service; managing keys needs a signed-in session
	k := apikeys.NewSystem(s.DB)
	k.IsAdmin = o.AdminModel().IsAdmin // service keys are the org's: admins make and revoke them (TL-462)
	mux.Handle("GET /api/keys", sessionRequired(http.HandlerFunc(k.List)))
	mux.Handle("POST /api/keys", sessionRequired(http.HandlerFunc(k.Create)))
	mux.Handle("DELETE /api/keys/{id}", sessionRequired(http.HandlerFunc(k.Revoke)))

	// Billing — answers 503 until Stripe is configured. The plan is the active org's; its admins buy and manage it.
	bill := s.Billing
	bill.IsAdmin = o.AdminModel().IsAdmin
	mux.Handle("GET /api/subscription", signedIn(bill.Status))
	mux.Handle("POST /api/subscription/checkout", signedIn(bill.Checkout))
	mux.Handle("POST /api/subscription/portal", signedIn(bill.Portal))
	mux.Handle("POST /api/subscription/plan", signedIn(bill.ChangePlan)) // Pro ↔ Enterprise, prorated (TL-290)
	mux.Handle("GET /api/plans", public("plans", 60, bill.Plans))        // limits and prices, for the homepage
	// Stripe authenticates webhooks by signature, not session
	mux.Handle("POST /api/webhooks/stripe", public("webhook-stripe", 600, s.Billing.Webhook(time.Now)))
	mux.Handle("POST /webhooks/stripe", public("webhook-stripe", 600, s.Billing.Webhook(time.Now))) // same path as ../bugfixes (stripe listen --forward-to …/webhooks/stripe)
	// Clerk (via Svix) authenticates by signature: org/user removals clean up admins, team members and API keys
	// Contact form: files enquiries in ChewedFeed HQ's inbox; a few per address per minute, and 20 a minute in all
	mux.Handle("POST /api/enquiries", ratelimit.Total(limits, "enquiry-all", 20, time.Now)(public("enquiry", 3, enquiries.NewSystem(config.Get(s.Config).ChewedFeedURL, config.Get(s.Config).ChewedFeedKey).Create)))
	mux.Handle("POST /api/webhooks/clerk", public("webhook-clerk", 600, clerkHooks(s.DB, config.Get(s.Config).ClerkWebhookSecret, config.Get(s.Config).ClerkUncappedOrgs).Webhook(time.Now)))

	// Every request: API key (tl_…) or optional Clerk auth + users row for signed-in callers, then
	// recovery/request id/logging (bugfixes) and CORS.
	cors := bm.NewMiddleware()
	cors.AddAllowedOrigins(parties...)
	cors.AddAllowedMethods(http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions)
	cors.AddAllowedHeaders("Authorization", "X-Requested-With", httpx.ZoneHeader) // Accept, Content-Type are library defaults
	var handler, mcpHandler http.Handler
	// MCP (Streamable HTTP) for bots: same auth as REST (tl_ key or Clerk session); each tool calls a REST route
	// in-process through handler, so MCP and REST can't drift apart
	mux.Handle("/mcp", required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mcpHandler.ServeHTTP(w, r) })))
	// BadKeys first: an address that keeps presenting invalid keys is refused before more key lookups
	handler = ratelimit.BadKeys(limits, 20, time.Now)(apikeys.Middleware(s.DB, keyMember)(ratelimit.Middleware(s.Valkey, config.Get(s.Config).RateLimit, time.Now)(optional(users.Ensure(s.DB)(
		bm.Recoverer(bm.RequestID(bm.Logger(cors.CORS(httpx.WithZone(mux))))))))))
	mcpHandler = mcp.Handler(handler, "1.0.0")
	// Google Chat signs its requests with a Google bearer token (TL-289), which the Clerk middleware
	// above would refuse as a bad session: that one route goes round it and checks Google's signature itself
	outer := http.NewServeMux()
	outer.Handle("POST /api/webhooks/google-chat", bm.Recoverer(public("webhook-google-chat", 600, chatApp.Events)))
	outer.Handle("/", handler)
	return httpx.SecurityHeaders(outer)
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.Ping(r.Context()); err != nil {
		http.Error(w, "db down", http.StatusServiceUnavailable)
		return
	}
	// Valkey only caches; the API works without it, so it doesn't fail the health check
	_, _ = w.Write([]byte("ok"))
}

// serve runs until SIGINT/SIGTERM, then drains in-flight requests before returning: returning is
// what lets main close the database, so it must wait (audit: deploys used to cut writes off mid-way).
func (s *Service) serve(ctx context.Context, h http.Handler) error {
	ln, err := net.Listen("tcp", ":"+s.Port)
	if err != nil {
		return err
	}
	logs.Logf("Listening on %s", ln.Addr())
	return serveOn(ctx, ln, h, 15*time.Second)
}

// serveOn serves on ln until ctx ends, then gives in-flight requests up to drain to finish.
func serveOn(ctx context.Context, ln net.Listener, h http.Handler, drain time.Duration) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		// SSE connections stay open; per-response write deadlines would close every live view.
		WriteTimeout: 0,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served: // failed to serve at all
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	err := srv.Shutdown(shutdown) // waits for in-flight requests (SSE streams end at the deadline)
	<-served
	if errors.Is(err, context.DeadlineExceeded) {
		logs.Warnf("shutdown: requests still running after %s", drain)
		return nil
	}
	return err
}

// actAs runs an API call on h as user, signed in to org, in-process (Google Chat buttons, TL-455): the
// caller has already checked who they are, so the route applies that person's own permissions.
func actAs(h http.Handler) func(ctx context.Context, user, org, method, path string, body any) (int, []byte) {
	return func(ctx context.Context, user, org, method, path string, body any) (int, []byte) {
		var in bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&in).Encode(body)
		}
		claims := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		claims.ActiveOrganizationID = org
		req := httptest.NewRequestWithContext(clerk.ContextWithSessionClaims(ctx, claims), method, path, &in)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
}

// stillMember asks Clerk (through the minute-long membership cache) whether user is still in org,
// for the API key middleware.
func stillMember(members clerkcache.Memberships) apikeys.Member {
	return func(ctx context.Context, org, user string) (bool, error) {
		limit := int64(1)
		list, err := members.List(ctx, &organizationmembership.ListParams{OrganizationID: org, UserIDs: []string{user}, ListParams: clerk.ListParams{Limit: &limit}})
		if err != nil || list == nil {
			return false, err
		}
		for _, m := range list.OrganizationMemberships {
			if m != nil && m.PublicUserData != nil && m.PublicUserData.UserID == user {
				return true, nil
			}
		}
		return false, nil
	}
}

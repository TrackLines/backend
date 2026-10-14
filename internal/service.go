package internal

import (
	"context"
	"errors"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	bm "github.com/bugfixes/go-bugfixes/middleware"
	goFlags "github.com/flags-gg/go-flags"
	"github.com/jackc/pgx/v5/pgxpool"
	ConfigBuilder "github.com/keloran/go-config"
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
	"github.com/valkey-io/valkey-go"
)

// Service owns the HTTP lifecycle and route composition.
// cmd/server is limited to configuration, migrations and dependency construction.
type Service struct {
	Config  *ConfigBuilder.Config
	DB      *pgxpool.Pool
	Valkey  valkey.Client   // optional (nil when unreachable at startup): caching only
	Flags   *goFlags.Client // s.Flags.Is("name").Enabled()
	Billing billing.Service
	Port    string
	// KeyMember overrides how API keys check their owner is still in the org (tests); nil asks Clerk
	KeyMember apikeys.Member
}

func New(cfg *ConfigBuilder.Config, db *pgxpool.Pool, vk valkey.Client, fl *goFlags.Client, bill billing.Service, port string) *Service {
	return &Service{Config: cfg, DB: db, Valkey: vk, Flags: fl, Billing: bill, Port: port}
}

// notifiers are the in-app inbox, Web Push (to people's devices, when the server has VAPID keys), and
// the two together: what everything that tells people something sends through.
func (s *Service) notifiers() (notifications.Store, *notifications.Push, notifications.Notifier) {
	inbox := notifications.Store{DB: s.DB}
	pc := config.Get(s.Config)
	push := notifications.NewPush(s.DB, notifications.PushConfig{PublicKey: pc.VAPIDPublicKey, PrivateKey: pc.VAPIDPrivateKey, Subject: pc.VAPIDSubject})
	var notify notifications.Notifier = inbox
	if push.Enabled() {
		notify = notifications.Multi{inbox, push}
	}
	return inbox, push, notify
}

func (s *Service) Start() error {
	// one shutdown signal for the server and background work, so auto-close stops before main
	// closes the database pool (it used to tick once more and log "closed pool" on every deploy)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// overdue sprints close themselves (manual close is POST /api/sprints/{id}/close); a next sprint
	// that starts over capacity tells the board's managers (TL-149)
	_, _, notify := s.notifiers()
	go sprints.Store{DB: s.DB, Notify: notify}.RunAutoClose(ctx, time.Minute)
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
	mux.Handle("PATCH /api/columns/{id}", signedIn(c.Update)) // name and/or wip_limit
	mux.Handle("DELETE /api/columns/{id}", signedIn(c.Delete))

	// Tickets
	// Notifications: an in-app inbox for now (external channels later implement the same Notifier)
	inbox, push, notify := s.notifiers()
	nf := notifications.NewSystem(inbox).WithPush(push)
	mux.Handle("GET /api/push", signedIn(nf.PushKey))
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
	mux.Handle("POST /api/columns/{id}/tickets", signedIn(t.Create))
	mux.Handle("GET /api/tickets/{id}", signedIn(t.Get))       // one ticket + where it lives (deep links)
	mux.Handle("GET /api/tickets/{id}/org", signedIn(t.Where)) // which of my orgs it's in, when it isn't in the active one
	mux.Handle("GET /api/search", signedIn(t.Search))          // full-text over titles, descriptions, comments; exact keys first
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
		BitbucketClientID:                                 gc.BitbucketClientID, BitbucketSecret: gc.BitbucketSecret, PublicAPI: gc.PublicAPIURL}).
		WithFlags(s.flagOn).                               // Bitbucket stays off until its flags.gg flag is on (TL-205)
		WithShortLinks(config.Get(s.Config).ShortLinkBase) // PRs can mention tickets by our short links (TL-283)
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

	// Sprints — per team board; closing opens the next and carries unfinished tickets over
	sp := sprints.NewSystem(s.DB, o.AdminModel()).WithLive(s.Valkey).WithNotifier(notify)
	lv := live.New(s.Valkey, boards.Store{DB: s.DB})
	lv.IsAdmin = o.AdminModel().IsAdmin
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
	mux.Handle("GET /api/boards/{id}/velocity", signedIn(sp.Velocity)) // per closed sprint + open sprint burn
	mux.Handle("GET /api/sprints/{id}", signedIn(sp.Get))              // read-only: burn data and tickets
	mux.Handle("PATCH /api/sprints/{id}", signedIn(sp.Update))         // change an open sprint's length and/or goal
	mux.Handle("POST /api/sprints/{id}/close", signedIn(sp.Close))
	// Refinement: up to 2 planned sprints per board; backlog tickets are planned and sized into them
	mux.Handle("GET /api/boards/{id}/planned-sprints", signedIn(sp.ListPlanned)) // with tickets and capacity (velocity + 10%)
	mux.Handle("POST /api/boards/{id}/planned-sprints", signedIn(sp.Plan))
	mux.Handle("PATCH /api/planned-sprints/{id}", signedIn(sp.UpdatePlanned)) // length and/or goal
	mux.Handle("DELETE /api/planned-sprints/{id}", signedIn(sp.Unplan))
	mux.Handle("POST /api/planned-sprints/{id}/approve", signedIn(sp.Approve)) // accept the total over capacity
	mux.Handle("PUT /api/tickets/{id}/planned-sprint", signedIn(sp.PlanTicket))

	// Request intake: a project's unlisted public form (no account) and the team's triage inbox
	ik := intake.NewSystem(s.DB)
	mux.Handle("GET /api/intake/{token}", public("intake-form", 60, ik.Form))
	mux.Handle("POST /api/intake/{token}", public("intake-submit", 10, ik.Submit)) // on top of the per-sender hourly cap
	mux.Handle("GET /api/projects/{id}/intake", signedIn(ik.Settings))
	mux.Handle("PUT /api/projects/{id}/intake", signedIn(ik.SetOpen))
	mux.Handle("GET /api/projects/{id}/intake/submissions", signedIn(ik.List))
	mux.Handle("POST /api/intake-submissions/{id}/promote", signedIn(ik.Promote))
	mux.Handle("POST /api/intake-submissions/{id}/dismiss", signedIn(ik.Dismiss))

	// Roadmaps — GET by id is open: public ones are readable by anyone with the link
	r := roadmaps.NewSystem(s.DB)
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
	mux.Handle("GET /api/keys", sessionRequired(http.HandlerFunc(k.List)))
	mux.Handle("POST /api/keys", sessionRequired(http.HandlerFunc(k.Create)))
	mux.Handle("DELETE /api/keys/{id}", sessionRequired(http.HandlerFunc(k.Revoke)))

	// Billing — answers 503 until Stripe is configured. The plan is the active org's; its admins buy and manage it.
	bill := s.Billing
	bill.IsAdmin = o.AdminModel().IsAdmin
	mux.Handle("GET /api/subscription", signedIn(bill.Status))
	mux.Handle("POST /api/subscription/checkout", signedIn(bill.Checkout))
	mux.Handle("POST /api/subscription/portal", signedIn(bill.Portal))
	// Stripe authenticates webhooks by signature, not session
	mux.Handle("POST /api/webhooks/stripe", public("webhook-stripe", 600, s.Billing.Webhook(time.Now)))
	mux.Handle("POST /webhooks/stripe", public("webhook-stripe", 600, s.Billing.Webhook(time.Now))) // same path as ../bugfixes (stripe listen --forward-to …/webhooks/stripe)
	// Clerk (via Svix) authenticates by signature: org/user removals clean up admins, team members and API keys
	// Contact form: files enquiries in ChewedFeed HQ's inbox; a few per address per minute, and 20 a minute in all
	mux.Handle("POST /api/enquiries", ratelimit.Total(limits, "enquiry-all", 20, time.Now)(public("enquiry", 3, enquiries.NewSystem(config.Get(s.Config).ChewedFeedURL, config.Get(s.Config).ChewedFeedKey).Create)))
	mux.Handle("POST /api/webhooks/clerk", public("webhook-clerk", 600, clerkhooks.NewSystem(s.DB, config.Get(s.Config).ClerkWebhookSecret).Webhook(time.Now)))

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
	return httpx.SecurityHeaders(handler)
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.Ping(r.Context()); err != nil {
		http.Error(w, "db down", http.StatusServiceUnavailable)
		return
	}
	// Valkey only caches; the API works without it, so it doesn't fail the health check
	_, _ = w.Write([]byte("ok"))
}

// serve runs until SIGINT/SIGTERM, then drains in-flight requests.
func (s *Service) serve(ctx context.Context, h http.Handler) error {
	srv := &http.Server{
		Addr:              ":" + s.Port,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		// SSE connections stay open; per-response write deadlines would close every live view.
		WriteTimeout: 0,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logs.Logf("Listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// flagOn reports a flags.gg flag; with no flags client (tests, local without IDs) everything flagged is off.
func (s *Service) flagOn(name string) bool { return s.Flags != nil && s.Flags.Is(name).Enabled() }

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

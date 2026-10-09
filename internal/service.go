package internal

import (
	"context"
	"errors"
	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
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
	"github.com/tracklines/backend/internal/bugfixes-tickets"
	"github.com/tracklines/backend/internal/clerkcache"
	"github.com/tracklines/backend/internal/columns"
	"github.com/tracklines/backend/internal/comments"
	"github.com/tracklines/backend/internal/config"
	"github.com/tracklines/backend/internal/invitations"
	"github.com/tracklines/backend/internal/mcp"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/projects"
	"github.com/tracklines/backend/internal/ratelimit"
	"github.com/tracklines/backend/internal/roadmaps"
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
}

func New(cfg *ConfigBuilder.Config, db *pgxpool.Pool, vk valkey.Client, fl *goFlags.Client, bill billing.Service, port string) *Service {
	return &Service{Config: cfg, DB: db, Valkey: vk, Flags: fl, Billing: bill, Port: port}
}

func (s *Service) Start() error {
	// overdue sprints close themselves (manual close is POST /api/sprints/{id}/close)
	go sprints.Store{DB: s.DB}.RunAutoClose(context.Background(), time.Minute)
	return s.serve(s.Handler())
}

// Handler is the whole API: routes plus the auth, logging and CORS middleware.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	signedIn := func(h http.HandlerFunc) http.Handler { return auth.Required(h) }
	// Clerk member lists for pickers come from Valkey for a minute; checks that guard changes ask Clerk
	members := clerkcache.Memberships{Next: organizationmembership.NewClient(&clerk.ClientConfig{}), VK: s.Valkey, TTL: time.Minute}
	o := organizations.NewSystem(s.DB).CachingMembers(members)
	mux.Handle("GET /api/organizations/members", signedIn(o.Members))
	mux.Handle("GET /api/organizations/me", signedIn(o.Me))             // the caller's admin flag and the teams they lead
	mux.Handle("GET /api/organizations/admins", signedIn(o.ListAdmins)) // first admin is resolved from Clerk on demand
	mux.Handle("PUT /api/organizations/admins/{userID}", signedIn(o.GrantAdmin))
	mux.Handle("DELETE /api/organizations/admins/{userID}", signedIn(o.RevokeAdmin)) // never the last admin
	iv := invitations.NewSystem(s.DB, o.AdminModel())
	mux.Handle("GET /api/invitations", auth.Optional(http.HandlerFunc(iv.Mine))) // invitations addressed to the signed-in user's verified emails
	mux.Handle("GET /api/organizations/invitations", signedIn(iv.List))
	mux.Handle("POST /api/organizations/invitations", signedIn(iv.Create))
	mux.Handle("GET /api/organizations/invitations/{id}", signedIn(iv.Get))
	mux.Handle("DELETE /api/organizations/invitations/{id}", signedIn(iv.Revoke))
	mux.Handle("POST /api/organizations/invitations/{id}/accept", auth.Optional(http.HandlerFunc(iv.Accept)))
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
	p := projects.NewSystem(s.DB)
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
	mux.Handle("PATCH /api/boards/{id}", signedIn(b.Update)) // name and estimate_scale settings
	mux.Handle("DELETE /api/boards/{id}", signedIn(b.Delete))

	// Columns
	c := columns.NewSystem(s.DB, o.AdminModel())
	mux.Handle("POST /api/boards/{boardID}/columns", signedIn(c.Create))
	mux.Handle("PUT /api/boards/{boardID}/columns/order", signedIn(c.Reorder))
	mux.Handle("PATCH /api/columns/{id}", signedIn(c.Update)) // name and/or wip_limit
	mux.Handle("DELETE /api/columns/{id}", signedIn(c.Delete))

	// Tickets
	t := tickets.NewSystem(s.DB).CachingMembers(members)
	mux.Handle("POST /api/columns/{id}/tickets", signedIn(t.Create))
	mux.Handle("GET /api/tickets/{id}", signedIn(t.Get)) // one ticket + where it lives (deep links)
	mux.Handle("PATCH /api/tickets/{id}", signedIn(t.Update))
	mux.Handle("POST /api/tickets/{id}/claim", signedIn(t.Claim))
	mux.Handle("POST /api/tickets/{id}/release", signedIn(t.Release))
	mux.Handle("PUT /api/tickets/{id}/assignee", signedIn(t.Assign))  // owner assigns to themself or an AI key
	mux.Handle("PUT /api/tickets/{id}/labels", signedIn(t.SetLabels)) // free-form labels; [] clears
	mux.Handle("GET /api/assignees", signedIn(t.Assignees))
	mux.Handle("DELETE /api/tickets/{id}", signedIn(t.Delete))
	mux.Handle("POST /api/tickets/{id}/move", signedIn(t.Move)) // also pulls a ticket out of the backlog
	mux.Handle("POST /api/tickets/{id}/backlog", signedIn(t.ToBacklog))
	mux.Handle("PUT /api/tickets/{id}/parent", signedIn(t.SetParent))        // sub-tickets; null detaches
	mux.Handle("PUT /api/tickets/{id}/blocked-by", signedIn(t.SetBlockedBy)) // dependencies; claim is refused while blocked

	// Comments — a ticket's conversation, separate from its description; replies via parent_comment_id
	cm := comments.NewSystem(s.DB)
	mux.Handle("GET /api/tickets/{id}/comments", signedIn(cm.List))
	mux.Handle("POST /api/tickets/{id}/comments", signedIn(cm.Create))

	// Attachments — files live on UploadThing (browser uploads via the Next.js route); we keep the records
	at := attachments.NewSystem(s.DB, attachments.UploadThing{Token: config.Get(s.Config).UploadThingToken})
	mux.Handle("GET /api/tickets/{id}/attachments", signedIn(at.List))
	mux.Handle("POST /api/tickets/{id}/attachments", signedIn(at.Create))
	mux.Handle("DELETE /api/attachments/{id}", signedIn(at.Delete))

	// Backlog — project tickets not on any board/sprint yet (e.g. triaged bugs)
	mux.Handle("GET /api/projects/{id}/backlog", signedIn(t.Backlog))
	mux.Handle("GET /api/projects/{id}/open-tickets", signedIn(t.OpenTickets)) // not done, boards + backlog, in pick order (MCP list_open_tickets)
	mux.Handle("GET /api/projects/{id}/backlog/page", signedIn(t.PageBacklog)) // paged + filtered, for the project page
	mux.Handle("POST /api/projects/{id}/backlog", signedIn(t.CreateBacklog))
	mux.Handle("GET /api/projects/{id}/labels", signedIn(t.ProjectLabels)) // labels in use, for suggestions

	// Sprints — per team board; closing opens the next and carries unfinished tickets over
	sp := sprints.NewSystem(s.DB, o.AdminModel())
	mux.Handle("GET /api/boards/{id}/sprints", signedIn(sp.List))
	mux.Handle("POST /api/boards/{id}/sprints", signedIn(sp.Start))
	mux.Handle("GET /api/boards/{id}/velocity", signedIn(sp.Velocity)) // per closed sprint + open sprint burn
	mux.Handle("GET /api/sprints/{id}", signedIn(sp.Get))              // read-only: burn data and tickets
	mux.Handle("POST /api/sprints/{id}/close", signedIn(sp.Close))

	// Roadmaps — GET by id is open: public ones are readable by anyone with the link
	r := roadmaps.NewSystem(s.DB)
	mux.HandleFunc("GET /api/roadmaps/{id}", r.Get)
	mux.Handle("GET /api/roadmaps", signedIn(r.List))
	mux.Handle("POST /api/projects/{id}/roadmaps", signedIn(r.Create))
	mux.Handle("PUT /api/roadmaps/{id}", signedIn(r.Update))
	mux.Handle("DELETE /api/roadmaps/{id}", signedIn(r.Delete))
	mux.Handle("PUT /api/roadmaps/{id}/items", signedIn(r.ReplaceItems))
	mux.Handle("PUT /api/roadmaps/{id}/items/{item}/tickets", signedIn(r.SetItemTickets)) // item progress = linked tickets done

	// API keys — AI agent or server/service; managing keys needs a signed-in session
	k := apikeys.NewSystem(s.DB)
	mux.Handle("GET /api/keys", auth.SessionRequired(http.HandlerFunc(k.List)))
	mux.Handle("POST /api/keys", auth.SessionRequired(http.HandlerFunc(k.Create)))
	mux.Handle("DELETE /api/keys/{id}", auth.SessionRequired(http.HandlerFunc(k.Revoke)))

	// Billing — answers 503 until Stripe is configured
	mux.Handle("GET /api/subscription", signedIn(s.Billing.Status))
	mux.Handle("POST /api/subscription/checkout", signedIn(s.Billing.Checkout))
	mux.Handle("POST /api/subscription/portal", signedIn(s.Billing.Portal))
	// Stripe authenticates webhooks by signature, not session
	mux.HandleFunc("POST /api/webhooks/stripe", s.Billing.Webhook(time.Now))
	mux.HandleFunc("POST /webhooks/stripe", s.Billing.Webhook(time.Now)) // same path as ../bugfixes (stripe listen --forward-to …/webhooks/stripe)

	// Bugfixes ticket-creation — auth via bf_ key (no Clerk session)
	bt := bugfixesTickets.NewSystem(s.DB)
	mux.Handle("POST /api/bugfixes/tickets", bugfixesTickets.Middleware(s.DB)(http.HandlerFunc(bt.Create)))

	// Every request: API key (tl_…) or bf_ key or optional Clerk auth + users row for signed-in callers, then
	// recovery/request id/logging (bugfixes) and CORS.
	cors := bm.NewMiddleware()
	cors.AddAllowedOrigins("http://localhost:3000", "https://tracklin.es")
	cors.AddAllowedMethods(http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions)
	cors.AddAllowedHeaders("Authorization", "X-Requested-With") // Accept, Content-Type are library defaults
	var handler, mcpHandler http.Handler
	// MCP (Streamable HTTP) for bots: same auth as REST (tl_ key or Clerk session); each tool calls a REST route
	// in-process through handler, so MCP and REST can't drift apart
	mux.Handle("/mcp", auth.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mcpHandler.ServeHTTP(w, r) })))
	handler = apikeys.Middleware(s.DB)(bugfixesTickets.Middleware(s.DB)(ratelimit.Middleware(s.Valkey, config.Get(s.Config).RateLimit, time.Now)(auth.Optional(users.Ensure(s.DB)(
		bm.Recoverer(bm.RequestID(bm.Logger(cors.CORS(mux)))))))))
	mcpHandler = mcp.Handler(handler, "1.0.0")
	return handler
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
func (s *Service) serve(h http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{
		Addr:              ":" + s.Port,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
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

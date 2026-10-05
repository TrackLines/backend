package internal

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	goFlags "github.com/flags-gg/go-flags"
	"github.com/jackc/pgx/v5/pgxpool"
	ConfigBuilder "github.com/keloran/go-config"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/columns"
	"github.com/tracklines/backend/internal/middleware"
	"github.com/tracklines/backend/internal/projects"
	"github.com/tracklines/backend/internal/roadmaps"
	"github.com/tracklines/backend/internal/tickets"
	"github.com/tracklines/backend/internal/users"
	"github.com/valkey-io/valkey-go"
)

// Service owns the HTTP lifecycle and route composition.
// cmd/server is limited to configuration, migrations and dependency construction.
type Service struct {
	Config  *ConfigBuilder.Config
	DB      *pgxpool.Pool
	Valkey  valkey.Client
	Flags   *goFlags.Client // s.Flags.Is("name").Enabled()
	Billing billing.Service
	Port    string
}

func New(cfg *ConfigBuilder.Config, db *pgxpool.Pool, vk valkey.Client, fl *goFlags.Client, bill billing.Service, port string) *Service {
	return &Service{Config: cfg, DB: db, Valkey: vk, Flags: fl, Billing: bill, Port: port}
}

func (s *Service) Start() error {
	mux := http.NewServeMux()
	signedIn := func(h http.HandlerFunc) http.Handler { return auth.Required(h) }

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
	b := boards.NewSystem(s.DB)
	mux.Handle("POST /api/projects/{id}/boards", signedIn(b.Create))
	mux.Handle("GET /api/boards/{id}", signedIn(b.Get))
	mux.Handle("DELETE /api/boards/{id}", signedIn(b.Delete))

	// Columns
	c := columns.NewSystem(s.DB)
	mux.Handle("POST /api/boards/{boardID}/columns", signedIn(c.Create))
	mux.Handle("PUT /api/boards/{boardID}/columns/order", signedIn(c.Reorder))
	mux.Handle("PATCH /api/columns/{id}", signedIn(c.Rename))
	mux.Handle("DELETE /api/columns/{id}", signedIn(c.Delete))

	// Tickets
	t := tickets.NewSystem(s.DB)
	mux.Handle("POST /api/columns/{id}/tickets", signedIn(t.Create))
	mux.Handle("PATCH /api/tickets/{id}", signedIn(t.Update))
	mux.Handle("DELETE /api/tickets/{id}", signedIn(t.Delete))
	mux.Handle("POST /api/tickets/{id}/move", signedIn(t.Move))

	// Roadmaps — GET by id is open: public ones are readable by anyone with the link
	r := roadmaps.NewSystem(s.DB)
	mux.HandleFunc("GET /api/roadmaps/{id}", r.Get)
	mux.Handle("GET /api/roadmaps", signedIn(r.List))
	mux.Handle("POST /api/projects/{id}/roadmaps", signedIn(r.Create))
	mux.Handle("PUT /api/roadmaps/{id}", signedIn(r.Update))
	mux.Handle("DELETE /api/roadmaps/{id}", signedIn(r.Delete))
	mux.Handle("PUT /api/roadmaps/{id}/items", signedIn(r.ReplaceItems))

	// Billing — answers 503 until Stripe is configured
	mux.Handle("GET /api/subscription", signedIn(s.Billing.Status))
	mux.Handle("POST /api/subscription/checkout", signedIn(s.Billing.Checkout))
	mux.Handle("POST /api/subscription/portal", signedIn(s.Billing.Portal))
	// Stripe authenticates webhooks by signature, not session
	mux.HandleFunc("POST /api/webhooks/stripe", s.Billing.Webhook(time.Now))

	// Every request: optional Clerk auth + users row for signed-in callers, then
	// recovery/request id/logging (bugfixes) and CORS.
	handler := auth.Optional(users.Ensure(s.DB)(middleware.Wrap(middleware.CORS([]string{"http://localhost:3000"})(mux))))
	return s.serve(handler)
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.Ping(r.Context()); err != nil {
		http.Error(w, "db down", http.StatusServiceUnavailable)
		return
	}
	if err := s.Valkey.Do(r.Context(), s.Valkey.B().Ping().Build()).Error(); err != nil {
		http.Error(w, "valkey down", http.StatusServiceUnavailable)
		return
	}
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

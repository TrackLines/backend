package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"

	"github.com/bugfixes/go-bugfixes/logs"
	goFlags "github.com/flags-gg/go-flags"
	"github.com/valkey-io/valkey-go"

	"github.com/tracklines/backend/inbox"
	service "github.com/tracklines/backend/internal"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/config"
	"github.com/tracklines/backend/internal/integrations"
	"github.com/tracklines/backend/internal/live"
	"github.com/tracklines/backend/internal/migrations"
	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/retention"
)

var (
	BuildVersion = "0.0.1"
	BuildHash    = "unknown"
	ServiceName  = "tracklines"
)

func run() error {
	c, err := config.Build()
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}
	pc := config.Get(c)
	// flags.gg: with no IDs set the SDK returns false, but FLAGS_<NAME>=true env overrides still work locally
	fl := goFlags.NewClient(goFlags.WithAuth(goFlags.Auth{
		ProjectID:     c.Flags.ProjectID,
		AgentID:       c.Flags.AgentID,
		EnvironmentID: c.Flags.EnvironmentID,
	}), goFlags.WithMemory())
	if fl == nil {
		return fmt.Errorf("init flags client")
	}
	auth.Init(c.Clerk.Key)

	db, err := c.Database.GetPGXPoolClient(context.Background())
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer db.Close()
	// migrate before serving, fail fast
	if err := migrations.Up(db); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if box, err := secrets.New(pc.SecretKey); err == nil {
		if err := (integrations.Store{DB: db, Box: box}).MigrateSealedSecrets(context.Background()); err != nil {
			return fmt.Errorf("migrate integration secrets: %w", err)
		}
	}

	vkOpt, err := valkey.ParseURL(pc.ValkeyURL)
	if err != nil {
		return fmt.Errorf("parse valkey url: %w", err)
	}
	// Valkey backs live views (standup, refinement) and the caches; the API still runs without it.
	// A deploy can start the API before Valkey is ready, so it's retried for about a minute.
	vk, err := live.Connect(10, time.Sleep, func() (valkey.Client, error) { return valkey.NewClient(vkOpt) })
	if err != nil {
		logs.Warnf("valkey unavailable, running without live views or caching until the next restart: %v", err)
		vk = nil
	} else {
		defer vk.Close()
	}

	port := strconv.Itoa(c.Local.HTTPPort)
	if pc.RailwayPort != "" {
		port = pc.RailwayPort
	}
	// a live Stripe key sends paying customers back to PORTAL_URL: localhost there is a misconfiguration
	if strings.HasPrefix(pc.Stripe.Key, "sk_live_") && strings.Contains(pc.PortalURL, "localhost") {
		logs.Warnf("PORTAL_URL is %q with a live Stripe key: set it to the site's settings page (e.g. https://tracklin.es/settings)", pc.PortalURL)
	}
	bill := billing.Service{
		DB:                db,
		Stripe:            billing.Stripe{SecretKey: pc.Stripe.Key},
		ProPriceID:        cmp.Or(pc.Stripe.ProID, pc.Stripe.PriceID), // STRIPE_PRICE_ID was Pro's before Enterprise (TL-290)
		EnterprisePriceID: pc.Stripe.EnterpriseID,
		WebhookSecret:     pc.Stripe.WebhookSecret,
		ReturnURL:         pc.PortalURL,
		ChewedFeed:        billing.ChewedFeed{URL: pc.ChewedFeedURL, Key: pc.ChewedFeedKey},
		ClerkOrgs:         organization.NewClient(&clerk.ClientConfig{}), // member caps follow the plan (TL-418)
	}
	return service.New(c, db, vk, fl, bill, port).Start()
}

// Railway cron jobs run this binary with a job name (TL-256): `purge` at 00:00 UTC (tickets 30 days
// in the bin, organizations deleted 30 days ago) and `sweep-files` at 00:30 UTC (deleted files whose
// day's batch is 30 days old). Each does its work and exits.
func job(name string) error {
	c, err := config.Build()
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}
	ctx := context.Background()
	db, err := c.Database.GetPGXPoolClient(ctx)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer db.Close()
	now := time.Now()
	switch name {
	case "purge": // retention.Purge: the automation service runs the same nightly (TL-431)
		r, err := retention.Purge(ctx, db, func(ctx context.Context, org string, to []string, n inbox.Notice) error {
			return notifications.Store{DB: db}.Notify(ctx, org, to, notifications.Event{Kind: n.Kind, Actor: n.Actor, TicketID: n.TicketID, Text: n.Text})
		}, now)
		if err != nil {
			return err
		}
		logs.Logf("purge: %s", r)
	case "sweep-files":
		n, err := retention.Sweeper{DB: db, Files: retention.UploadThing{Token: config.Get(c).UploadThingToken}}.SweepAll(ctx)
		logs.Logf("sweep-files: deleted %d file(s) from UploadThing", n)
		if err != nil {
			return fmt.Errorf("delete files from UploadThing: %w", err)
		}
	default:
		return fmt.Errorf("unknown job %q: purge or sweep-files", name)
	}
	return nil
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "/server" { // a start command of "/server purge" on top of the image's ENTRYPOINT
		args = args[1:]
	}
	if len(args) > 0 {
		if err := job(args[0]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "%s %s: %v\n", ServiceName, args[0], err)
			os.Exit(1)
		}
		return
	}
	logs.Logf("Starting %s version %s (build %s)", ServiceName, BuildVersion, BuildHash)
	if err := run(); err != nil {
		logs.Local().Warnf("%s stopped: %v", ServiceName, err)
		_, _ = fmt.Fprintf(os.Stderr, "%s stopped: %v\n", ServiceName, err)
		os.Exit(1)
	}
}

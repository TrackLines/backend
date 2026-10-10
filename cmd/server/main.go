package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	goFlags "github.com/flags-gg/go-flags"
	service "github.com/tracklines/backend/internal"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/billing"
	"github.com/tracklines/backend/internal/config"
	"github.com/tracklines/backend/internal/migrations"
	"github.com/valkey-io/valkey-go"
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

	vkOpt, err := valkey.ParseURL(pc.ValkeyURL)
	if err != nil {
		return fmt.Errorf("parse valkey url: %w", err)
	}
	// Valkey only caches, so the API runs without it (it isn't retried until the next restart)
	vk, err := valkey.NewClient(vkOpt)
	if err != nil {
		logs.Warnf("valkey unavailable, running without the cache: %v", err)
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
		DB:            db,
		Stripe:        billing.Stripe{SecretKey: pc.Stripe.Key},
		PriceID:       pc.Stripe.PriceID,
		WebhookSecret: pc.Stripe.WebhookSecret,
		ReturnURL:     pc.PortalURL,
	}
	return service.New(c, db, vk, fl, bill, port).Start()
}

func main() {
	logs.Logf("Starting %s version %s (build %s)", ServiceName, BuildVersion, BuildHash)
	if err := run(); err != nil {
		logs.Local().Warnf("%s stopped: %v", ServiceName, err)
		_, _ = fmt.Fprintf(os.Stderr, "%s stopped: %v\n", ServiceName, err)
		os.Exit(1)
	}
}

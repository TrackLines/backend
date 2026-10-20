package internal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	ConfigBuilder "github.com/keloran/go-config"
	healthcheck "github.com/keloran/go-healthcheck"
	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/valkey-io/valkey-go"

	"github.com/tracklines/backend/internal/config"
)

// Healthy only when Postgres and Valkey are: both answering, our pool able to query, and a connected
// Valkey client. Anything else is 503, so Railway holds the deploy back; internal hostnames stay out.
func TestHealthNeedsItsDependencies(t *testing.T) {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	container, err := tcvalkey.Run(ctx, "valkey/valkey:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	vkURL, _ := container.ConnectionString(ctx)
	opt, _ := valkey.ParseURL(vkURL)
	vk, err := valkey.NewClient(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer vk.Close()
	cfg := &ConfigBuilder.Config{ProjectConfig: &config.Project{ValkeyURL: vkURL}}

	check := func(s *Service) (int, healthcheck.Health, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.health(rec, httptest.NewRequest("GET", "/health", nil))
		var h healthcheck.Health
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatalf("body: %v %q", err, rec.Body.String())
		}
		return rec.Code, h, rec.Body.String()
	}
	status := func(h healthcheck.Health) map[string]healthcheck.HealthStatus {
		m := map[string]healthcheck.HealthStatus{}
		for _, d := range h.Dependencies {
			m[d.Name] = d.Status
		}
		return m
	}

	code, h, body := check(&Service{Config: cfg, DB: db, Valkey: vk})
	if code != 200 || h.Status != healthcheck.HealthPass || status(h)["postgres"] != "pass" || status(h)["valkey"] != "pass" {
		t.Fatalf("healthy: %d %s", code, body)
	}
	host := strings.Split(strings.TrimPrefix(vkURL, "redis://"), ":")[0]
	if strings.Contains(body, host+":") || strings.Contains(body, "postgres://") {
		t.Fatalf("internal addresses in the public body: %s", body)
	}

	// Valkey answering, but the API never connected to it (it was down at startup)
	if code, h, body = check(&Service{Config: cfg, DB: db}); code != 503 || status(h)["valkey"] != "fail" || status(h)["postgres"] != "pass" {
		t.Fatalf("valkey not connected: %d %s", code, body)
	}

	// Postgres unreachable
	bad := &ConfigBuilder.Config{ProjectConfig: cfg.ProjectConfig}
	bad.Database.Details.Host, bad.Database.Details.Port = "127.0.0.1", 1
	if code, h, body = check(&Service{Config: bad, DB: db, Valkey: vk}); code != 503 || h.Status != healthcheck.HealthFail {
		t.Fatalf("postgres unreachable: %d %s", code, body)
	}

	// Valkey down
	if err := container.Stop(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if code, h, body = check(&Service{Config: cfg, DB: db, Valkey: vk}); code != 503 || h.Status != healthcheck.HealthFail {
		t.Fatalf("valkey down: %d %s", code, body)
	}
}

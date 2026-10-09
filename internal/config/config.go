package config

import (
	bugfixes "github.com/bugfixes/go-bugfixes"
	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/caarlos0/env/v8"
	ConfigBuilder "github.com/keloran/go-config"
)

type Stripe struct {
	Key           string `env:"STRIPE_KEY"`
	WebhookSecret string `env:"STRIPE_WEBHOOK_SECRET"`
	PriceID       string `env:"STRIPE_PRICE_ID"`
}

// Project is the tracklines-specific config; go-config owns Postgres, Clerk, Bugfixes, Flags, Local.
type Project struct {
	Stripe           Stripe
	PortalURL        string `env:"PORTAL_URL" envDefault:"http://localhost:3000/settings"`
	ValkeyURL        string `env:"VALKEY_URL" envDefault:"redis://localhost:6379"`
	RailwayPort      string `env:"PORT"` // Railway injects PORT; overrides HTTP_PORT
	BugfixesLogLevel string `env:"BUGFIXES_LOG_LEVEL" envDefault:"info"`
	UploadThingToken string `env:"UPLOADTHING_TOKEN"`                   // ticket attachments; used to delete stored files
	RateLimit        int    `env:"API_KEY_RATE_LIMIT" envDefault:"300"` // requests per minute per API key; 0 turns it off
}

type ProjectConfigurator struct{}

func (ProjectConfigurator) Build(cfg *ConfigBuilder.Config) error {
	p := &Project{}
	if err := env.Parse(p); err != nil {
		return logs.Errorf("failed to parse project config: %v", err)
	}
	cfg.ProjectConfig = p
	return nil
}

func Build() (*ConfigBuilder.Config, error) {
	c := ConfigBuilder.NewConfigNoVault()
	if err := c.Build(
		ConfigBuilder.Local,
		ConfigBuilder.Postgres,
		ConfigBuilder.Clerk,
		ConfigBuilder.Bugfixes,
		ConfigBuilder.Flags,
		ConfigBuilder.WithProjectConfigurator(ProjectConfigurator{})); err != nil {
		return nil, err
	}
	// Share go-config's BugFixes credentials with SDK package-level logging.
	bugfixesConfig := bugfixes.Config{
		Server:      c.Bugfixes.Server,
		AgentKey:    c.Bugfixes.AgentKey,
		AgentSecret: c.Bugfixes.AgentSecret,
		LogLevel:    Get(c).BugfixesLogLevel,
		LocalOnly:   c.Local.KeepLocal,
	}
	bugfixes.SetDefaultConfig(bugfixesConfig)

	// go-config's logs.Local() is always local-only. Replace it with an SDK
	// logger configured from the same credentials and local-only setting.
	logger := &logs.BugFixes{}
	logger.Setup(c.Bugfixes.AgentKey, c.Bugfixes.AgentSecret)
	logger.SetConfig(bugfixesConfig)
	c.Bugfixes.Logger = logger
	return c, nil
}

// Get returns the typed project config; Build must have succeeded first.
func Get(cfg *ConfigBuilder.Config) *Project {
	p, _ := ConfigBuilder.GetProjectConfig[Project](cfg)
	if p == nil {
		return &Project{} // nothing configured: optional integrations stay off
	}
	return p
}

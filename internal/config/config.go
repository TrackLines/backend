package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	bugfixes "github.com/bugfixes/go-bugfixes"
	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/caarlos0/env/v8"
	"github.com/joho/godotenv"
	"github.com/keloran/go-config/v2/auth/clerk"
	configbugfixes "github.com/keloran/go-config/v2/bugfixes"
	"github.com/keloran/go-config/v2/database/postgres"
	"github.com/keloran/go-config/v2/flags"
	"github.com/keloran/go-config/v2/local"
	"github.com/keloran/go-config/v2/onepassword"
)

// Config is the backend's: go-config's packages for what it uses (no all-in-one root package, so
// Mongo, Keycloak, Rabbit, Influx and Vault stay out of the binary), plus its own settings.
type Config struct {
	Local    local.System
	Database postgres.System
	Clerk    clerk.System
	Bugfixes configbugfixes.System
	Flags    flags.System
	Project  Project
}

type Stripe struct {
	Key           string `env:"STRIPE_KEY"`
	WebhookSecret string `env:"STRIPE_WEBHOOK_SECRET"`
	PriceID       string `env:"STRIPE_PRICE_ID"`      // the old name for STRIPE_PRO_ID, still read if that isn't set
	ProID         string `env:"STRIPE_PRO_ID"`        // Pro: 5 projects (TL-290); a price_… or prod_… id
	EnterpriseID  string `env:"STRIPE_ENTERPRISE_ID"` // Enterprise: unlimited projects
}

// Git is the GitHub App and Bitbucket OAuth consumer; a provider with missing settings stays off.
type Git struct {
	GitHubAppID         string `env:"GITHUB_APP_ID"`
	GitHubAppSlug       string `env:"GITHUB_APP_SLUG"`        // github.com/apps/<slug>
	GitHubPrivateKey    string `env:"GITHUB_APP_PRIVATE_KEY"` // the app's PEM key (newlines as \n are fine)
	GitHubWebhookSecret string `env:"GITHUB_WEBHOOK_SECRET"`
	GitHubClientID      string `env:"GITHUB_CLIENT_ID"` // the App's OAuth client: proves who installed it
	GitHubClientSecret  string `env:"GITHUB_CLIENT_SECRET"`
	BitbucketClientID   string `env:"BITBUCKET_CLIENT_ID"`
	BitbucketSecret     string `env:"BITBUCKET_CLIENT_SECRET"`
	PublicAPIURL        string `env:"PUBLIC_API_URL" envDefault:"https://api.tracklin.es"` // where providers call back (Bitbucket hooks Tracklines creates per repo)
}

// Project is the tracklines-specific config; go-config owns Postgres, Clerk, Bugfixes, Flags, Local.
type Project struct {
	Stripe                 Stripe
	PortalURL              string   `env:"PORTAL_URL" envDefault:"http://localhost:3000/settings"`
	ClerkAuthorizedParties string   `env:"CLERK_AUTHORIZED_PARTIES" envDefault:"https://tracklin.es"` // docker-compose adds http://localhost:3000 for local dev
	ValkeyURL              string   `env:"VALKEY_URL" envDefault:"redis://localhost:6379"`
	RailwayPort            string   `env:"PORT"` // Railway injects PORT; overrides HTTP_PORT
	BugfixesLogLevel       string   `env:"BUGFIXES_LOG_LEVEL" envDefault:"info"`
	ClerkWebhookSecret     string   `env:"CLERK_WEBHOOK_SECRET"`                 // whsec_…; /api/webhooks/clerk answers 503 without it
	ClerkUncappedOrgs      []string `env:"CLERK_UNCAPPED_ORGS" envSeparator:","` // org ids whose Clerk member cap is never set (TL-418)
	UploadThingToken       string   `env:"UPLOADTHING_TOKEN"`                    // ticket attachments; used to delete stored files
	AuditURL               string   `env:"AUDIT_URL"`                            // the auditor on the private network (TL-666); events stay queued without it
	AuditSecret            string   `env:"AUDIT_SECRET"`                         // its bearer token (1Password audit_backend)
	SecretKey              string   `env:"TRACKLINES_SECRET_KEY"`                // 32 bytes base64: seals integration tokens; integrations stay off without it
	Git                    Git
	RateLimit              int `env:"API_KEY_RATE_LIMIT" envDefault:"300"` // requests per minute per API key; 0 turns it off
	// how many right-most X-Forwarded-For hops our infrastructure writes, so we don't trust a
	// caller-supplied address (httpx.ClientIP). 1 = a single trusted edge (default). 0 = the origin is
	// reachable directly (local Compose), so ignore X-Forwarded-For and use the connection address.
	TrustedProxyHops int    `env:"TRUSTED_PROXY_HOPS" envDefault:"1"`
	ChewedFeedURL    string `env:"CHEWEDFEED_API_URL" envDefault:"https://api.chewedfeed.com"` // ChewedFeed HQ (customer sync, enquiries)
	ChewedFeedKey    string `env:"CHEWEDFEED_ENQUIRY_KEY"`                                     // tracklines' enquiry key in ChewedFeed; sync and enquiries are off without it
	// short links for "Copy link" (TL-277), through 1tn
	ShortLinkAPI  string `env:"SHORT_LINK_API" envDefault:"https://api.1tn.pw"`
	ShortLinkBase string `env:"SHORT_LINK_BASE" envDefault:"https://1tn.pw"`
	// Web Push (notifications with Tracklines closed): `npx web-push generate-vapid-keys`; off without them
	VAPIDPublicKey  string `env:"VAPID_PUBLIC_KEY"`
	VAPIDPrivateKey string `env:"VAPID_PRIVATE_KEY"`
	VAPIDSubject    string `env:"VAPID_SUBJECT" envDefault:"mailto:hello@tracklin.es"`
	// Google Chat app (TL-289): personal messages in Chat. The app's service account key (the JSON file
	// Google downloads); off without it. Project number: only if the app is built as a Workspace add-on.
	GoogleChatCredentials   string `env:"GOOGLE_CHAT_CREDENTIALS"`
	GoogleChatProjectNumber string `env:"GOOGLE_CHAT_PROJECT_NUMBER"`
	// the admin panel (TL-573): members of this Clerk org (ChewedFeed) are staff; nobody without it.
	// Staging sets its own; production's comes from 1Password.
	StaffOrgID string `env:"STAFF_ORG_ID"`
}

// AuthorizedParties returns the origins allowed in Clerk session token azp claims.
func (p *Project) AuthorizedParties() []string {
	var parties []string
	for _, party := range strings.Split(p.ClerkAuthorizedParties, ",") {
		if party = strings.TrimSpace(party); party != "" {
			parties = append(parties, party)
		}
	}
	return parties
}

// secrets are the env vars loaded from 1Password's tracklines vault at boot (TL-526), "<item>.<field>"
// with an item per service. Nothing without OP_SERVICE_ACCOUNT_TOKEN, and a set variable wins.
var secrets = map[string]string{
	"bitbucket_backend.username":   "BITBUCKET_CLIENT_ID",
	"bitbucket_backend.credential": "BITBUCKET_CLIENT_SECRET",
	"bugfixes_backend.username":    "BUGFIXES_AGENT_KEY",
	"bugfixes_backend.credential":  "BUGFIXES_AGENT_SECRET",
	"bugfixes_backend.service":     "BUGFIXES_SERVICE", // read by go-config's bugfixes from its next release
	"chewedfeed_backend.username":  "CHEWEDFEED_ENQUIRY_KEY",
	"clerk_backend.username":       "CLERK_SECRET_KEY",
	"clerk_backend.credential":     "CLERK_WEBHOOK_SECRET",
	"flags_backend.username":       "FLAGS_AGENT_ID",
	"flags_backend.credential":     "FLAGS_ENVIRONMENT_ID",
	"flags_backend.project":        "FLAGS_PROJECT_ID",
	"github_backend.username":      "GITHUB_APP_ID",
	"github_backend.credential":    "GITHUB_APP_PRIVATE_KEY",
	"github_backend.slug":          "GITHUB_APP_SLUG",
	"github_backend.client_id":     "GITHUB_CLIENT_ID",
	"github_backend.client_secret": "GITHUB_CLIENT_SECRET",
	"github_backend.webhook":       "GITHUB_WEBHOOK_SECRET",
	"google_backend.username":      "GOOGLE_CHAT_PROJECT_NUMBER",
	"google_backend.credential":    "GOOGLE_CHAT_CREDENTIALS",
	"staff_backend.username":       "STAFF_ORG_ID",
	"stripe_backend.credential":    "STRIPE_KEY",
	"stripe_backend.webhook":       "STRIPE_WEBHOOK_SECRET",
	"stripe_backend.enterprise":    "STRIPE_ENTERPRISE_ID",
	"stripe_backend.pro":           "STRIPE_PRO_ID",
	"tracklines_backend.username":  "TRACKLINES_SECRET_KEY",
	"vapid_backend.username":       "VAPID_PUBLIC_KEY",
	"vapid_backend.credential":     "VAPID_PRIVATE_KEY",
	"uploadthing_backend.username": "UPLOADTHING_TOKEN",
	"audit_backend.credential":     "AUDIT_SECRET",
}

// Build reads .env (if there is one; the environment wins), then each part's settings, then sends
// logs and errors to Bugfixes with them.
func Build() (*Config, error) {
	if err := loadDotEnv(); err != nil {
		return nil, err
	}
	if err := onepassword.Load("tracklines", secrets); err != nil {
		return nil, fmt.Errorf("1password: %w", err)
	}
	l, err := local.Build()
	if err != nil {
		return nil, fmt.Errorf("config: unable to build local: %w", err)
	}
	db := postgres.NewSystem()
	if _, err := db.Build(); err != nil {
		return nil, fmt.Errorf("database: unable to build: %w", err)
	}
	ck := clerk.NewSystem()
	if _, err := ck.Build(); err != nil {
		return nil, fmt.Errorf("clerk: unable to build: %w", err)
	}
	bf := configbugfixes.NewSystem()
	if _, err := bf.Build(); err != nil {
		return nil, fmt.Errorf("bugfixes: unable to build: %w", err)
	}
	fl, err := flags.Build()
	if err != nil {
		return nil, fmt.Errorf("flags: unable to build: %w", err)
	}
	c := &Config{Local: *l, Database: *db, Clerk: *ck, Bugfixes: *bf, Flags: *fl}
	if err := env.Parse(&c.Project); err != nil {
		return nil, fmt.Errorf("failed to parse project config: %w", err)
	}

	// Share go-config's BugFixes credentials with SDK package-level logging.
	bugfixesConfig := bugfixes.Config{
		Server:      c.Bugfixes.Server,
		AgentKey:    c.Bugfixes.AgentKey,
		AgentSecret: c.Bugfixes.AgentSecret,
		LogLevel:    c.Project.BugfixesLogLevel,
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

// loadDotEnv loads .env when there is one, as go-config's root package did; set variables win.
func loadDotEnv() error {
	if _, err := os.Stat(".env"); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to stat .env: %w", err)
	}
	if err := godotenv.Load(".env"); err != nil {
		return fmt.Errorf("failed to load .env: %w", err)
	}
	return nil
}

// Get returns the project settings; nil (nothing configured, in tests) is the defaults-off zero value.
func Get(cfg *Config) *Project {
	if cfg == nil {
		return &Project{} // nothing configured: optional integrations stay off
	}
	return &cfg.Project
}

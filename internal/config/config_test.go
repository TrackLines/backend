package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuild(t *testing.T) {
	t.Setenv("STRIPE_PRICE_ID", "price_123")
	c, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	if p := Get(c); p == nil || p.Stripe.PriceID != "price_123" {
		t.Fatalf("project config not parsed: %+v", p)
	}
	// there's one ChewedFeed API; only the enquiry key turns customer sync and enquiries on
	if got := Get(c).ChewedFeedURL; got != "https://api.chewedfeed.com" {
		t.Fatalf("default ChewedFeed URL = %q", got)
	}
	if got, want := Get(c).AuthorizedParties(), []string{"https://tracklin.es"}; !equalStrings(got, want) {
		t.Fatalf("default authorized parties = %q, want %q", got, want)
	}
}

// Each go-config part reads its settings, .env fills in what the environment doesn't set, and a set
// variable wins over .env, as with go-config's root package before (go-config v2).
func TestBuildReadsEachPartAndDotEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("RDS_HOSTNAME=from-dotenv\nFLAGS_PROJECT_ID=flags-from-dotenv\nCLERK_SECRET_KEY=sk_dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("CLERK_SECRET_KEY", "sk_env") // the environment wins over .env
	t.Setenv("HTTP_PORT", "8081")
	t.Setenv("BUGFIXES_AGENT_KEY", "agent")
	t.Setenv("BUGFIXES_LOG_LEVEL", "warn")
	for _, k := range []string{"RDS_HOSTNAME", "FLAGS_PROJECT_ID"} {
		t.Setenv(k, "")
		os.Unsetenv(k) // only .env sets these
	}
	c, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Details.Host != "from-dotenv" || c.Flags.ProjectID != "flags-from-dotenv" {
		t.Fatalf(".env not loaded: host %q, flags %q", c.Database.Details.Host, c.Flags.ProjectID)
	}
	if c.Clerk.Key != "sk_env" {
		t.Fatalf("the environment should win over .env: %q", c.Clerk.Key)
	}
	if c.Local.HTTPPort != 8081 || c.Bugfixes.AgentKey != "agent" || c.Bugfixes.Logger == nil || c.Project.BugfixesLogLevel != "warn" {
		t.Fatalf("parts: port %d, agent %q, logger %v, level %q", c.Local.HTTPPort, c.Bugfixes.AgentKey, c.Bugfixes.Logger != nil, c.Project.BugfixesLogLevel)
	}
	if Get(nil) == nil || Get(c) != &c.Project {
		t.Fatal("Get")
	}
}

func TestAuthorizedPartiesFromConfig(t *testing.T) {
	p := &Project{ClerkAuthorizedParties: " https://app.example, http://localhost:3000 ,,"}
	if got, want := p.AuthorizedParties(), []string{"https://app.example", "http://localhost:3000"}; !equalStrings(got, want) {
		t.Fatalf("authorized parties = %q, want %q", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

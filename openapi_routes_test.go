package backend

import (
	"context"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// undocumented are routes the spec leaves out on purpose.
var undocumented = []string{
	"POST /webhooks/stripe", // the same handler as /api/webhooks/stripe, for `stripe listen --forward-to`
}

// TL-316: the native apps generate their API clients from openapi.yaml, so every route in
// internal/service.go is documented, every documented path is served, and the spec is valid.
func TestOpenAPICoversRoutes(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	spec, err := loader.LoadFromData(OpenAPIYAML)
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(ctx); err != nil {
		t.Fatalf("openapi.yaml isn't valid: %v", err)
	}

	src, err := os.ReadFile("internal/service.go")
	if err != nil {
		t.Fatal(err)
	}
	// "GET /api/x/{id}" patterns; one without a method ("/mcp") serves every method
	served := map[string]bool{}
	for _, m := range regexp.MustCompile(`Handle(?:Func)?\("(?:([A-Z]+) )?(/[^"\s]*)"`).FindAllStringSubmatch(string(src), -1) {
		served[route(m[1], m[2])] = true
	}

	documented := map[string]bool{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			base := spec.Servers
			if len(item.Servers) > 0 {
				base = item.Servers
			}
			if op.Servers != nil && len(*op.Servers) > 0 {
				base = *op.Servers
			}
			u, err := url.Parse(base[0].URL)
			if err != nil {
				t.Fatalf("%s %s: server %q: %v", method, path, base[0].URL, err)
			}
			r := route(method, strings.TrimSuffix(u.Path, "/")+path)
			documented[r] = true
			if !served[r] && !served[route("", strings.TrimSuffix(u.Path, "/")+path)] {
				t.Errorf("openapi.yaml documents %s, which internal/service.go doesn't serve", r)
			}
		}
	}
	for r := range served {
		if strings.HasPrefix(r, "* ") || documented[r] || slices.Contains(undocumented, r) {
			continue
		}
		t.Errorf("%s is served but not in openapi.yaml", r)
	}
	for r := range served { // a method-less route still needs some documented method; "/" is the catch-all
		if p, ok := strings.CutPrefix(r, "* "); ok && p != "" && !slices.ContainsFunc(slices.Collect(maps.Keys(documented)), func(d string) bool { return strings.HasSuffix(d, " "+p) }) {
			t.Errorf("%s is served but not in openapi.yaml", p)
		}
	}
}

// route is "METHOD /path" with path parameter names dropped, so {id} and {ticketId} match.
func route(method, path string) string {
	if method == "" {
		method = "*"
	}
	return strings.ToUpper(method) + " " + regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(strings.TrimSuffix(path, "/"), "{}")
}

package integrations

import (
	"net/url"
	"strings"

	"github.com/tracklines/backend/internal/safehttp"
)

// externalClient is for calling URLs an org admin typed in (a self-hosted Bugfixes): public
// addresses only, no redirects (TL-161). Shared with webhooks (TL-635).
var externalClient = safehttp.Client

// validBaseURL: an https origin and nothing else, so paths we append can't be steered with a query,
// fragment or credentials.
func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && !strings.ContainsAny(raw, "?#") &&
		(u.Path == "" || u.Path == "/")
}

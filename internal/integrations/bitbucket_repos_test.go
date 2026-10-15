package integrations

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bitbucket repos come workspace by workspace, every page (TL-286): the cross-workspace
// /repositories?role=member is retired. A "next" link off the API host isn't followed, and a
// refusal says why in words (logs strip numbers).
func TestBitbucketReposByWorkspace(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"type":"error","error":{"message":"Access token expired"}}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/2.0/user/workspaces":
			_, _ = w.Write([]byte(`{"values":[{"workspace":{"slug":"chewedfeed"}},{"workspace":{"slug":"1tn"}}]}`))
		case r.URL.Path == "/2.0/repositories/chewedfeed" && r.URL.Query().Get("page") == "":
			_, _ = w.Write([]byte(`{"values":[{"full_name":"chewedfeed/web","mainbranch":{"name":"trunk"}}],"next":"` + srv.URL + `/2.0/repositories/chewedfeed?page=2"}`))
		case r.URL.Path == "/2.0/repositories/chewedfeed" && r.URL.Query().Get("page") == "2":
			_, _ = w.Write([]byte(`{"values":[{"full_name":"chewedfeed/api","is_private":true}]}`))
		case r.URL.Path == "/2.0/repositories/1tn":
			_, _ = w.Write([]byte(`{"values":[{"full_name":"1tn/short"}],"next":"https://evil.example/steal"}`))
		case r.URL.Path == "/2.0/repositories": // retired
			http.Error(w, `{"type":"error","error":{"message":"This API is no longer supported."}}`, http.StatusGone)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := Git{BitbucketAPI: srv.URL + "/2.0"}

	repos, err := g.bitbucketRepos(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.FullName+"@"+r.DefaultBranch)
	}
	if got := strings.Join(names, ","); got != "1tn/short@main,chewedfeed/web@trunk,chewedfeed/api@main" {
		t.Fatalf("repos: %s", got)
	}
	// a refusal names its status and the provider's reason
	_, err = g.bitbucketRepos(context.Background(), "expired")
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") || !strings.Contains(err.Error(), "Access token expired") {
		t.Fatalf("refusal: %v", err)
	}
}

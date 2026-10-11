package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/secrets"
)

// ProjectRepo is a repository linked to a project: PRs in it can link to the project's tickets.
type ProjectRepo struct {
	Provider      string `json:"provider"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	CreatedAt     string `json:"created_at"`
}

var (
	ErrRepoNotVisible = errors.New("the connection can't see that repository")
	ErrRepoLinked     = errors.New("that repository is already linked to a project in this organization")
)

func (s Store) projectRepos(ctx context.Context, org, project string) ([]ProjectRepo, error) {
	rows, err := s.DB.Query(ctx, `SELECT r.provider, r.full_name, r.default_branch, to_char(r.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM project_repos r JOIN projects p ON p.id = r.project_id AND p.owner_clerk_id = $1
		WHERE r.project_id = $2 ORDER BY r.provider, r.full_name`, org, project)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ProjectRepo, error) {
		var r ProjectRepo
		return r, row.Scan(&r.Provider, &r.FullName, &r.DefaultBranch, &r.CreatedAt)
	})
	if out == nil {
		out = []ProjectRepo{}
	}
	return out, err
}

func (s Store) ownsProject(ctx context.Context, org, project string) bool {
	var ok bool
	_ = s.DB.QueryRow(ctx, `SELECT true FROM projects WHERE id::text = $1 AND owner_clerk_id = $2`, project, org).Scan(&ok)
	return ok
}

// HTTP: connecting

func (h System) providerParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.PathValue("provider")
	if p != GitHub && p != Bitbucket {
		http.Error(w, "provider must be github or bitbucket", http.StatusNotFound)
		return "", false
	}
	return p, true
}

func gitErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrProviderOff), errors.Is(err, secrets.ErrNoKey):
		http.Error(w, "this integration isn't configured on the server", http.StatusServiceUnavailable)
	case errors.Is(err, ErrNotConnected):
		http.Error(w, "not connected", http.StatusConflict)
	case errors.Is(err, ErrBadState), errors.Is(err, ErrRepoNotVisible):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrRepoLinked):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrProviderCall):
		logs.Warnf("integrations: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		logs.Errorf("integrations: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ConnectURL: GET /api/integrations/{provider}/connect → {"url"}: where an org admin goes to
// install the GitHub App or authorize Bitbucket. The provider sends them back with a state.
func (h System) ConnectURL(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.providerParam(w, r)
	if !ok {
		return
	}
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	u, err := h.connectURL(org, provider)
	if err != nil {
		gitErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"url": u})
}

// Complete: POST /api/integrations/{provider}/complete {"state", "installation_id" (GitHub) | "code" (Bitbucket)}.
func (h System) Complete(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.providerParam(w, r)
	if !ok {
		return
	}
	org, user, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		State          string `json:"state"`
		InstallationID string `json:"installation_id"`
		Code           string `json:"code"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err := h.complete(r.Context(), org, user, provider, in.State, in.InstallationID, in.Code); err != nil {
		gitErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListRepos: GET /api/integrations/{provider}/repos: what the connection can see, to pick from.
func (h System) ListRepos(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.providerParam(w, r)
	if !ok {
		return
	}
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	repos, err := h.Repos(r.Context(), org, provider)
	if err != nil {
		gitErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, repos)
}

// HTTP: project repos

// ProjectRepos: GET /api/projects/{id}/repos. Any org member.
func (h System) ProjectRepos(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if !h.store.ownsProject(r.Context(), org, r.PathValue("id")) {
		http.NotFound(w, r)
		return
	}
	out, err := h.store.projectRepos(r.Context(), org, r.PathValue("id"))
	if err != nil {
		gitErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// LinkRepo: POST /api/projects/{id}/repos {"provider", "full_name"}. Org admins; only repos the
// org's connection can see.
func (h System) LinkRepo(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	project := r.PathValue("id")
	if !h.store.ownsProject(r.Context(), org, project) {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Provider string `json:"provider"`
		FullName string `json:"full_name"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Provider != GitHub && in.Provider != Bitbucket {
		http.Error(w, "provider must be github or bitbucket", http.StatusBadRequest)
		return
	}
	repos, err := h.Repos(r.Context(), org, in.Provider)
	if err != nil {
		gitErr(w, err)
		return
	}
	var repo *Repo
	for i := range repos {
		if strings.EqualFold(repos[i].FullName, strings.TrimSpace(in.FullName)) {
			repo = &repos[i]
		}
	}
	if repo == nil {
		gitErr(w, ErrRepoNotVisible)
		return
	}
	_, err = h.store.DB.Exec(r.Context(), `INSERT INTO project_repos (project_id, org_id, provider, full_name, default_branch) VALUES ($1, $2, $3, $4, $5)`,
		project, org, in.Provider, repo.FullName, repo.DefaultBranch)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		gitErr(w, ErrRepoLinked)
		return
	}
	if err != nil {
		gitErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, ProjectRepo{Provider: in.Provider, FullName: repo.FullName, DefaultBranch: repo.DefaultBranch})
}

// UnlinkRepo: DELETE /api/projects/{id}/repos?provider=&full_name=. Org admins.
func (h System) UnlinkRepo(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.admins.RequireAdmin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	tag, err := h.store.DB.Exec(r.Context(), `DELETE FROM project_repos WHERE project_id::text = $1 AND org_id = $2 AND provider = $3 AND full_name = $4`,
		r.PathValue("id"), org, q.Get("provider"), q.Get("full_name"))
	if err != nil {
		gitErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HTTP: webhooks (no session: authenticated by signature)

// verifyHub checks a "sha256=<hex>" HMAC of body (GitHub's X-Hub-Signature-256, Bitbucket's X-Hub-Signature).
func verifyHub(body []byte, header, secret string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || secret == "" {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// GitHubWebhook: POST /api/webhooks/github. Pull request events link PRs to the tickets they
// mention (and merging closes them); uninstalling the app disconnects the org.
func (h System) GitHubWebhook(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.Error(w, "github webhooks aren't configured", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
		if err != nil || !verifyHub(body, r.Header.Get("X-Hub-Signature-256"), secret) {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		if pr, installation, ok := githubPR(r.Header.Get("X-GitHub-Event"), body); ok {
			orgs, err := h.orgsFor(r.Context(), GitHub, installation, pr.Repo)
			if err == nil {
				err = h.linkPR(r.Context(), orgs, pr)
			}
			if err != nil {
				gitErr(w, err) // 5xx: GitHub redelivers
				return
			}
		}
		if r.Header.Get("X-GitHub-Event") == "installation" {
			var e struct {
				Action       string `json:"action"`
				Installation struct {
					ID json.Number `json:"id"`
				} `json:"installation"`
			}
			if json.Unmarshal(body, &e) == nil && e.Action == "deleted" && e.Installation.ID != "" {
				if _, err := h.store.DB.Exec(r.Context(), `DELETE FROM org_integrations WHERE provider = 'github' AND external_id = $1`, e.Installation.ID.String()); err != nil {
					gitErr(w, err)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// BitbucketWebhook: POST /api/webhooks/bitbucket (X-Hub-Signature with the webhook secret).
func (h System) BitbucketWebhook(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.Error(w, "bitbucket webhooks aren't configured", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
		if err != nil || !verifyHub(body, r.Header.Get("X-Hub-Signature"), secret) {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		if pr, ok := bitbucketPR(r.Header.Get("X-Event-Key"), body); ok {
			orgs, err := h.orgsFor(r.Context(), Bitbucket, "", pr.Repo)
			if err == nil {
				err = h.linkPR(r.Context(), orgs, pr)
			}
			if err != nil {
				gitErr(w, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

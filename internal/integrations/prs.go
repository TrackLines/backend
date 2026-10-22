package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/comments"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/tickets"
)

// PullRequest is a provider's PR, as both webhooks describe it.
type PullRequest struct {
	Provider string `json:"provider"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	State    string `json:"state"` // open | draft | merged | closed
	Author   string `json:"author"`
	Branch   string `json:"branch"`
	body     string
	// installation: the GitHub App installation the event came through, for fetching its commits
	installation string
	mergeSHA string // GitHub's merge commit, once merged: a release that contains it ships the PR (TL-531)
}

// LinkedPR is a PR linked to a ticket.
type LinkedPR struct {
	PullRequest
	UpdatedAt  string  `json:"updated_at"`
	ReleasedIn *string `json:"released_in"` // the release tag that shipped it
}

var keyInText = regexp.MustCompile(`(?i)\b([a-z][a-z0-9]{1,5})-([0-9]+)\b`)

// keysIn finds ticket numbers in text that use prefix (the linked project's key prefix), so
// unrelated tokens like UTF-8 don't match.
func keysIn(prefix string, texts ...string) []int {
	seen := map[int]bool{}
	var out []int
	for _, t := range texts {
		for _, m := range keyInText.FindAllStringSubmatch(t, -1) {
			if !strings.EqualFold(m[1], prefix) {
				continue
			}
			if n, err := strconv.Atoi(m[2]); err == nil && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

var ticketURL = regexp.MustCompile(`/tickets/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b`)

// mentioned is the tickets in project that texts mention (TL-283): by key (TL-123), by link
// (…/tickets/<id>), or by one of our short links (1tn.pw/<code>, looked up in short_links, so only
// links Tracklines made). Only project's own tickets count, so a link can't reach another org's.
func (h System) mentioned(ctx context.Context, project, prefix string, texts ...string) ([]string, error) {
	numbers := keysIn(prefix, texts...)
	ids := []string{}
	var codes []string
	for _, t := range texts {
		for _, m := range ticketURL.FindAllStringSubmatch(t, -1) {
			ids = append(ids, strings.ToLower(m[1]))
		}
		if h.shortLink != nil {
			for _, m := range h.shortLink.FindAllStringSubmatch(t, -1) {
				codes = append(codes, m[1])
			}
		}
	}
	if len(codes) > 0 {
		rows, err := h.store.DB.Query(ctx, `SELECT url FROM short_links WHERE short = ANY($1)`, codes)
		if err != nil {
			return nil, err
		}
		longs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, long := range longs {
			if m := ticketURL.FindStringSubmatch(long); m != nil {
				ids = append(ids, strings.ToLower(m[1]))
			}
		}
	}
	if len(numbers) == 0 && len(ids) == 0 {
		return nil, nil
	}
	rows, err := h.store.DB.Query(ctx, `SELECT id::text FROM tickets WHERE project_id = $1 AND (number = ANY($2) OR id::text = ANY($3)) ORDER BY number`,
		project, numbers, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// WithShortLinks lets PRs mention tickets by our short links (base, e.g. https://1tn.pw).
func (h System) WithShortLinks(base string) System {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		h.shortLink = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(u.Host) + `/([A-Za-z0-9]{4,16})\b`)
	}
	return h
}

// linkPR records pr on the tickets it mentions in each project its repo is linked to (in the
// orgs that link it), and finishes those tickets when the PR has just been merged. Its commit
// messages count as mentions too (TL-513), fetched once a linked repo needs them.
func (h System) linkPR(ctx context.Context, orgs []string, pr PullRequest) error {
	var commits []string
	fetched := false
	for _, org := range orgs {
		var project, prefix string
		err := h.store.DB.QueryRow(ctx, `SELECT r.project_id, p.key_prefix FROM project_repos r JOIN projects p ON p.id = r.project_id
			WHERE r.org_id = $1 AND r.provider = $2 AND lower(r.full_name) = lower($3)`, org, pr.Provider, pr.Repo).Scan(&project, &prefix)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // not a linked repo in this org
		}
		if err != nil {
			return err
		}
		if !fetched {
			commits, fetched = h.commitMessages(ctx, org, pr), true
		}
		mentioned, err := h.mentioned(ctx, project, prefix, append([]string{pr.Branch, pr.Title, pr.body}, commits...)...)
		if err != nil {
			return err
		}
		for _, ticket := range mentioned {
			var was *string
			_ = h.store.DB.QueryRow(ctx, `SELECT state FROM ticket_prs WHERE ticket_id = $1 AND provider = $2 AND repo = $3 AND number = $4`,
				ticket, pr.Provider, pr.Repo, pr.Number).Scan(&was)
			if _, err := h.store.DB.Exec(ctx, `INSERT INTO ticket_prs (ticket_id, provider, repo, number, url, title, state, author, branch, merge_sha)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''))
				ON CONFLICT (ticket_id, provider, repo, number) DO UPDATE SET url = $5, title = $6, state = $7, author = $8, branch = $9,
					merge_sha = COALESCE(NULLIF($10, ''), ticket_prs.merge_sha), updated_at = now()`,
				ticket, pr.Provider, pr.Repo, pr.Number, pr.URL, pr.Title, pr.State, pr.Author, pr.Branch, pr.mergeSHA); err != nil {
				return err
			}
			if pr.State == "merged" && (was == nil || *was != "merged") { // once, on the merge (redeliveries are no-ops)
				if err := h.finishByPR(ctx, org, ticket, pr); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// commitMessages is the PR's commit messages. A failed fetch is logged and gives none: the PR still
// links from its branch, title and description.
func (h System) commitMessages(ctx context.Context, org string, pr PullRequest) []string {
	var msgs []string
	var token string
	var err error
	switch pr.Provider {
	case GitHub:
		if token, err = h.git.githubToken(ctx, pr.installation); err == nil {
			msgs, err = h.git.githubCommits(ctx, token, pr.Repo, pr.Number)
		}
	case Bitbucket:
		if token, err = h.bitbucketAccess(ctx, org); err == nil {
			msgs, err = h.git.bitbucketCommits(ctx, token, pr.Repo, pr.Number)
		}
	}
	if err != nil {
		logs.Warnf("integrations: commits of %s %s#%d: %v", pr.Provider, pr.Repo, pr.Number, err)
	}
	return msgs
}

// finishByPR closes a ticket whose PR merged: to its board's Done column, or resolved in the
// backlog, with a comment saying which PR. On a board with a Dev complete column, a GitHub PR's
// merge moves it there instead, and the release that ships it finishes it (releasePRs).
func (h System) finishByPR(ctx context.Context, org, ticket string, pr PullRequest) error {
	column, msg := "", fmt.Sprintf("Closed by %s#%d, merged: [%s](%s)", pr.Repo, pr.Number, pr.Title, pr.URL)
	if pr.mergeSHA != "" { // ponytail: GitHub only; Bitbucket has no releases, so its merges still finish tickets
		var dev *string
		if err := h.store.DB.QueryRow(ctx, `SELECT b.dev_complete_column_id::text FROM tickets t JOIN boards b ON b.id = t.board_id WHERE t.id = $1`,
			ticket).Scan(&dev); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if dev != nil {
			column, msg = *dev, fmt.Sprintf("Merged in %s#%d: [%s](%s). Done when a release ships it.", pr.Repo, pr.Number, pr.Title, pr.URL)
		}
	}
	return h.settle(ctx, org, ticket, column, pr.Provider, msg)
}

// settle moves an unfinished ticket to column ("": its board's Done column, or resolved in the
// backlog) and comments why. Already-done tickets are left alone.
func (h System) settle(ctx context.Context, org, ticket, column, author, msg string) error {
	ts := tickets.Store{DB: h.store.DB}
	d, err := ts.Get(ctx, org, ticket)
	if err != nil || d.Done {
		return err
	}
	switch {
	case column != "":
		err = ts.Move(ctx, org, ticket, column, 0)
	case d.BoardID == nil:
		err = ts.Resolve(ctx, org, ticket)
	default:
		if err = h.store.DB.QueryRow(ctx, `SELECT id FROM columns WHERE board_id = $1 ORDER BY position DESC LIMIT 1`, *d.BoardID).Scan(&column); err == nil {
			err = ts.Move(ctx, org, ticket, column, 0)
		}
	}
	if err != nil {
		return err
	}
	_, err = comments.Store{DB: h.store.DB}.Create(ctx, org, author, ticket, msg, nil)
	return err
}

// Release is a published GitHub release.
type Release struct {
	Repo, Tag, URL string
}

// githubRelease reads a release event that published a (non-pre)release; ok false otherwise.
func githubRelease(event string, body []byte) (rel Release, installation string, ok bool) {
	if event != "release" {
		return rel, "", false
	}
	var e struct {
		Action  string `json:"action"`
		Release struct {
			TagName    string `json:"tag_name"`
			HTMLURL    string `json:"html_url"`
			Prerelease bool   `json:"prerelease"`
		} `json:"release"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID json.Number `json:"id"`
		} `json:"installation"`
	}
	// ponytail: prereleases don't ship (they aren't deployed); count them if a board ever wants that
	if json.Unmarshal(body, &e) != nil || e.Action != "published" || e.Release.Prerelease || e.Release.TagName == "" {
		return rel, "", false
	}
	return Release{Repo: e.Repository.FullName, Tag: e.Release.TagName, URL: e.Release.HTMLURL}, e.Installation.ID.String(), true
}

// releasePRs marks the merged, unreleased PRs in orgs that rel's tag contains as released in it,
// then finishes each of their tickets that has no merged PR still waiting for a release. Each PR
// is compared with the tag itself, so out-of-order releases and redeliveries are fine.
func (h System) releasePRs(ctx context.Context, orgs []string, installation string, rel Release) error {
	if len(orgs) == 0 {
		return nil
	}
	rows, err := h.store.DB.Query(ctx, `SELECT DISTINCT pr.merge_sha FROM ticket_prs pr JOIN tickets t ON t.id = pr.ticket_id
		JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = ANY($2)
		WHERE pr.provider = 'github' AND pr.repo = $1 AND pr.merge_sha IS NOT NULL AND pr.released_at IS NULL`, rel.Repo, orgs)
	if err != nil {
		return err
	}
	shas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(shas) == 0 {
		return err
	}
	token, err := h.git.githubToken(ctx, installation)
	if err != nil {
		return err
	}
	type shipped struct{ org, ticket string }
	var done []shipped
	// ponytail: one compare call per unreleased merge; fine while each release ships tens, not thousands
	for _, sha := range shas {
		in, err := h.git.githubContains(ctx, token, rel.Repo, sha, rel.Tag)
		if err != nil {
			return err
		}
		if !in {
			continue
		}
		rows, err := h.store.DB.Query(ctx, `UPDATE ticket_prs pr SET released_in = $3, released_at = now()
			FROM tickets t JOIN projects p ON p.id = t.project_id
			WHERE t.id = pr.ticket_id AND p.owner_clerk_id = ANY($4) AND pr.provider = 'github' AND pr.repo = $1 AND pr.merge_sha = $2 AND pr.released_at IS NULL
			RETURNING p.owner_clerk_id, pr.ticket_id::text`, rel.Repo, sha, rel.Tag, orgs)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (shipped, error) {
			var s shipped
			return s, row.Scan(&s.org, &s.ticket)
		})
		if err != nil {
			return err
		}
		done = append(done, got...)
	}
	for _, s := range done {
		var waiting bool
		if err := h.store.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ticket_prs WHERE ticket_id = $1 AND merge_sha IS NOT NULL AND released_at IS NULL)`,
			s.ticket).Scan(&waiting); err != nil {
			return err
		}
		if waiting {
			continue
		}
		if err := h.settle(ctx, s.org, s.ticket, "", GitHub, fmt.Sprintf("Released in %s %s: %s", rel.Repo, rel.Tag, rel.URL)); err != nil {
			return err
		}
	}
	return nil
}

// githubPR reads a pull_request event; ok false for other events.
func githubPR(event string, body []byte) (pr PullRequest, installation string, ok bool) {
	if event != "pull_request" {
		return pr, "", false
	}
	var e struct {
		PullRequest struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			State   string `json:"state"`
			Draft   bool   `json:"draft"`
			Merged  bool   `json:"merged"`
			SHA     string `json:"merge_commit_sha"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID json.Number `json:"id"`
		} `json:"installation"`
	}
	if json.Unmarshal(body, &e) != nil || e.PullRequest.Number == 0 {
		return pr, "", false
	}
	p := e.PullRequest
	state, sha := "open", ""
	switch {
	case p.Merged:
		state, sha = "merged", p.SHA
	case p.State == "closed":
		state = "closed"
	case p.Draft:
		state = "draft"
	}
	return PullRequest{Provider: GitHub, Repo: e.Repository.FullName, Number: p.Number, URL: p.HTMLURL, Title: p.Title,
		State: state, Author: p.User.Login, Branch: p.Head.Ref, body: p.Body, installation: e.Installation.ID.String(), mergeSHA: sha}, e.Installation.ID.String(), true
}

// bitbucketPR reads a pullrequest:* event; ok false for other events.
func bitbucketPR(event string, body []byte) (PullRequest, bool) {
	if !strings.HasPrefix(event, "pullrequest:") {
		return PullRequest{}, false
	}
	var e struct {
		PullRequest struct {
			ID          int    `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			State       string `json:"state"` // OPEN, MERGED, DECLINED, SUPERSEDED
			Draft       bool   `json:"draft"`
			Author      struct {
				DisplayName string `json:"display_name"`
			} `json:"author"`
			Source struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
			} `json:"source"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
		} `json:"pullrequest"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if json.Unmarshal(body, &e) != nil || e.PullRequest.ID == 0 {
		return PullRequest{}, false
	}
	p := e.PullRequest
	state := "open"
	switch {
	case p.State == "MERGED":
		state = "merged"
	case p.State == "DECLINED" || p.State == "SUPERSEDED":
		state = "closed"
	case p.Draft:
		state = "draft"
	}
	return PullRequest{Provider: Bitbucket, Repo: e.Repository.FullName, Number: p.ID, URL: p.Links.HTML.Href, Title: p.Title,
		State: state, Author: p.Author.DisplayName, Branch: p.Source.Branch.Name, body: p.Description}, true
}

// orgsFor: a GitHub event names the installation; the org connected through it (one: TL-158).
// Bitbucket deliveries name their link instead (BitbucketWebhook).
func (h System) orgsFor(ctx context.Context, provider, installation, repo string) ([]string, error) {
	rows, err := h.store.DB.Query(ctx, `SELECT org_id FROM org_integrations WHERE provider = 'github' AND external_id = $1`, installation)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// TicketPRs: GET /api/tickets/{id}/prs: PRs linked to the ticket, newest first.
func (h System) TicketPRs(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.DB.Query(r.Context(), `SELECT pr.provider, pr.repo, pr.number, pr.url, pr.title, pr.state, pr.author, pr.branch,
			to_char(pr.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), pr.released_in
		FROM ticket_prs pr JOIN tickets t ON t.id = pr.ticket_id JOIN projects p ON p.id = t.project_id AND p.owner_clerk_id = $2
		WHERE pr.ticket_id::text = $1 ORDER BY pr.updated_at DESC`, r.PathValue("id"), auth.OrgID(r.Context()))
	if err != nil {
		logs.Errorf("integrations: ticket prs: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LinkedPR, error) {
		var p LinkedPR
		return p, row.Scan(&p.Provider, &p.Repo, &p.Number, &p.URL, &p.Title, &p.State, &p.Author, &p.Branch, &p.UpdatedAt, &p.ReleasedIn)
	})
	if err != nil {
		logs.Errorf("integrations: ticket prs: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []LinkedPR{}
	}
	httpx.JSON(w, http.StatusOK, out)
}

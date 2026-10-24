package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/secrets"
	"github.com/tracklines/backend/internal/tickets"
	"github.com/valkey-io/valkey-go"
)

// Report is the part of a Bugfixes report a ticket shows.
type Report struct {
	Title       string   `json:"title"`
	Occurrences int      `json:"occurrences"`
	Status      string   `json:"status"` // open | resolved | ignored
	LastSeen    string   `json:"last_seen"`
	Levels      []string `json:"levels"`
	File        string   `json:"file"`
	Line        int      `json:"line"`
}

// reportSource fetches a report; the HTTP one calls Bugfixes, tests use a fake.
type reportSource interface {
	Report(ctx context.Context, org, project, id string) (*Report, error)
}

type ticketSource interface {
	ExternalOf(ctx context.Context, owner, id string) (*tickets.External, *time.Time, error)
}

// BugfixesAPI reads reports from Bugfixes with the org's access token, caching them in Valkey for a
// minute (VK may be nil: no cache) so opening tickets doesn't hammer Bugfixes.
type BugfixesAPI struct {
	Store  Store
	VK     valkey.Client
	TTL    time.Duration
	Client *http.Client
}

var errUnavailable = errors.New("bugfixes unavailable")

func (b BugfixesAPI) Report(ctx context.Context, org, project, id string) (*Report, error) {
	key := fmt.Sprintf("tracklines:bugfixes:%s:%s:%s", org, project, id)
	if b.VK != nil {
		if raw, err := b.VK.Do(ctx, b.VK.B().Get().Key(key).Build()).AsBytes(); err == nil {
			var r Report
			if json.Unmarshal(raw, &r) == nil {
				return &r, nil
			}
		}
	}
	base, token, err := b.Store.credentials(ctx, org, Bugfixes)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/v1/projects/"+url.PathEscape(project)+"/reports/"+url.PathEscape(id)+"?occurrence_page_size=1&communication_page_size=1", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := b.Client
	if client == nil {
		client = externalClient(5 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", errUnavailable, resp.StatusCode)
	}
	var body struct {
		Report struct {
			Title       string   `json:"title"`
			Occurrences int      `json:"occurrences"`
			Status      string   `json:"status"`
			LastSeen    string   `json:"last_seen"`
			Levels      []string `json:"levels"`
		} `json:"report"`
		StaticAnalysis struct {
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"static_analysis"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	r := &Report{Title: body.Report.Title, Occurrences: body.Report.Occurrences, Status: body.Report.Status,
		LastSeen: body.Report.LastSeen, Levels: body.Report.Levels, File: body.StaticAnalysis.File, Line: body.StaticAnalysis.Line}
	if r.Levels == nil {
		r.Levels = []string{}
	}
	if b.VK != nil {
		if raw, err := json.Marshal(r); err == nil {
			ttl := b.TTL
			if ttl == 0 {
				ttl = time.Minute
			}
			_ = b.VK.Do(ctx, b.VK.B().Set().Key(key).Value(string(raw)).Ex(ttl).Build()).Error()
		}
	}
	return r, nil
}

// NewSystem: the integration handlers. Reports come from Bugfixes through the org's token; git is
// the server's GitHub App / Bitbucket consumer.
func NewSystem(db *pgxpool.Pool, box secrets.Box, vk valkey.Client, admins organizations.Admins, git Git) System {
	store := Store{DB: db, Box: box}
	git.GitHubPrivateKey = pemText(git.GitHubPrivateKey)
	return System{store: store, admins: admins, reports: BugfixesAPI{Store: store, VK: vk}, tickets: tickets.Store{DB: db}, git: git, now: time.Now}
}

// TicketReport is a ticket's Bugfixes panel. Available false means it's linked but the details
// couldn't be fetched (reason: not_connected | unavailable); the link still works.
type TicketReport struct {
	Source    string  `json:"source"`
	Project   string  `json:"project"`
	ReportID  string  `json:"report_id"`
	URL       string  `json:"url"`
	Available bool    `json:"available"`
	Reason    string  `json:"reason,omitempty"`
	Report    *Report `json:"report,omitempty"`
	Regressed bool    `json:"regressed"` // the crash was seen again after the ticket was done
}

// TicketReport: GET /api/tickets/{id}/bugfixes. 404 when the ticket didn't come from Bugfixes.
func (h System) TicketReport(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	ext, done, err := h.tickets.ExternalOf(r.Context(), org, r.PathValue("id"))
	if errors.Is(err, tickets.ErrNotFound) || (err == nil && ext.Source != Bugfixes) {
		http.Error(w, "this ticket has no Bugfixes report", http.StatusNotFound)
		return
	}
	if err != nil {
		logs.Errorf("integrations: ticket external: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := TicketReport{Source: ext.Source, Project: ext.Project, ReportID: ext.ID, URL: ext.URL}
	rep, err := h.reports.Report(r.Context(), org, ext.Project, ext.ID)
	switch {
	case errors.Is(err, ErrNotConnected), errors.Is(err, secrets.ErrNoKey):
		out.Reason = "not_connected"
	case err != nil:
		logs.Warnf("integrations: bugfixes report %s/%s: %v", ext.Project, ext.ID, err)
		out.Reason = "unavailable"
	default:
		out.Available, out.Report = true, rep
		out.Regressed = regressed(rep, done)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// regressed: the ticket was finished, but Bugfixes saw the crash after that and it's open again.
func regressed(r *Report, done *time.Time) bool {
	if done == nil || r.Status != "open" {
		return false
	}
	seen, err := time.Parse(time.RFC3339Nano, r.LastSeen)
	return err == nil && seen.After(*done)
}

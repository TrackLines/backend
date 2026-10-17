// Package legal tells everyone when the Terms of service or the Privacy policy change (TL-424).
// The documents and their versions live in the web app, which lists them at /legal-versions.json;
// this reads that list and, the first time it sees a version, sends every user an in-app notice.
package legal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Version is one published version, as the web app lists it.
type Version struct {
	Doc     string `json:"doc"`     // terms | privacy
	Version string `json:"version"` // the date it took effect, 2026-10-17
	Summary string `json:"summary"` // what changed, in a sentence
}

var names = map[string]string{"terms": "Terms of service", "privacy": "Privacy policy"}

// Announcer checks the web app's list and announces new versions.
type Announcer struct {
	DB     *pgxpool.Pool
	Site   string // the web app, e.g. https://tracklin.es
	Client *http.Client
}

// Run checks every interval until ctx ends.
func (a Announcer) Run(ctx context.Context, every time.Duration) {
	if a.Site == "" {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := a.Check(ctx); err != nil && ctx.Err() == nil {
			logs.Warnf("legal: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check reads the list once and announces what's new. The very first time (nothing recorded yet),
// everything listed is recorded without a notice: the documents as they were at launch.
func (a Announcer) Check(ctx context.Context) error {
	versions, err := a.versions(ctx)
	if err != nil {
		return err
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// one checker at a time, across instances
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('legal-announcements'))`); err != nil {
		return err
	}
	var seen int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM legal_announcements`).Scan(&seen); err != nil {
		return err
	}
	first := seen == 0
	for _, v := range versions {
		if names[v.Doc] == "" || len(v.Version) != len("2006-01-02") {
			continue
		}
		tag, err := tx.Exec(ctx, `INSERT INTO legal_announcements (doc, version) VALUES ($1, $2) ON CONFLICT DO NOTHING`, v.Doc, v.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 || first {
			continue // already announced, or the launch versions
		}
		if err := notifyEveryone(ctx, tx, v); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// notifyEveryone sends each user a notice in no particular organization (org_id ”), so it shows
// in their bell whichever organization they're in.
func notifyEveryone(ctx context.Context, tx pgx.Tx, v Version) error {
	text := fmt.Sprintf("We've updated our %s (from %s): %s", names[v.Doc], v.Version, strings.TrimSpace(v.Summary))
	if r := []rune(text); len(r) > 500 {
		text = string(r[:499]) + "…"
	}
	_, err := tx.Exec(ctx, `INSERT INTO notifications (org_id, user_id, kind, text, link)
		SELECT '', clerk_id, 'legal', $1, $2 FROM users`, text, "/"+v.Doc+"/"+v.Version)
	return err
}

func (a Announcer) versions(ctx context.Context) ([]Version, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.Site, "/")+"/legal-versions.json", nil)
	if err != nil {
		return nil, err
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("legal versions: %d", res.StatusCode)
	}
	var out []Version
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out)
	return out, err
}

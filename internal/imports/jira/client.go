package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrUnauthorized = httpx.Code("jira_unauthorized", "Jira refused that email and API token")
	ErrUnreachable  = httpx.Code("jira_unreachable", "couldn't reach Jira at that address")
)

// Client is Jira Cloud's REST API (v3, and the Agile API for boards) for one site, as one person
// (their email and API token).
type Client struct {
	Site  string // https://<name>.atlassian.net
	Email string
	Token string
	HTTP  *http.Client
	sleep func(time.Duration) // waiting out a rate limit (tests: no wait)
}

const tries = 5 // a rate-limited or briefly unavailable request is retried this many times in all

// Get reads path (with query) into out. A 429 or 503 is retried after Jira's Retry-After, or 1s, 2s,
// 4s… without one; a refused token is ErrUnauthorized.
func (c Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	u := strings.TrimRight(c.Site, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	sleep := c.sleep
	if sleep == nil {
		sleep = func(d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	for try := 1; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.Email, c.Token)
		req.Header.Set("Accept", "application/json")
		res, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		switch {
		case res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable:
			if try == tries {
				return fmt.Errorf("jira: %s still rate limited after %d tries", path, tries)
			}
			wait := time.Duration(1<<(try-1)) * time.Second
			if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s >= 0 {
				wait = min(time.Duration(s)*time.Second, time.Minute)
			}
			sleep(wait)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
			return ErrUnauthorized
		case res.StatusCode >= 300:
			return fmt.Errorf("jira: %s: %s", path, res.Status)
		}
		return json.Unmarshal(body, out)
	}
}

// List calls each with every item of a startAt-paged list: the Agile API's {values, isLast} and the
// platform's {values|comments|…, total}, the items under key.
func (c Client) List(ctx context.Context, path string, query url.Values, key string, each func(json.RawMessage) error) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	start := 0
	for {
		q.Set("startAt", strconv.Itoa(start))
		var raw map[string]json.RawMessage
		if err := c.Get(ctx, path, q, &raw); err != nil {
			return err
		}
		var total *int
		var last *bool
		var items []json.RawMessage
		_ = json.Unmarshal(raw["total"], &total)
		_ = json.Unmarshal(raw["isLast"], &last)
		if err := json.Unmarshal(raw[key], &items); err != nil && raw[key] != nil {
			return fmt.Errorf("jira: %s: %w", path, err)
		}
		for _, item := range items {
			if err := each(item); err != nil {
				return err
			}
		}
		start += len(items)
		switch {
		case len(items) == 0, last != nil && *last, last == nil && total != nil && start >= *total:
			return nil
		}
	}
}

// Search calls each with every issue a JQL search finds (query: jql, fields, maxResults), paging
// with Jira's nextPageToken (the enhanced search, /rest/api/3/search/jql).
func (c Client) Search(ctx context.Context, query url.Values, each func(json.RawMessage) error) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	for {
		var res struct {
			Issues        []json.RawMessage `json:"issues"`
			NextPageToken string            `json:"nextPageToken"`
			IsLast        bool              `json:"isLast"`
		}
		if err := c.Get(ctx, "/rest/api/3/search/jql", q, &res); err != nil {
			return err
		}
		for _, is := range res.Issues {
			if err := each(is); err != nil {
				return err
			}
		}
		if res.IsLast || res.NextPageToken == "" || len(res.Issues) == 0 {
			return nil
		}
		q.Set("nextPageToken", res.NextPageToken)
	}
}

// Myself is who the token belongs to: checks the connection works, and names it.
func (c Client) Myself(ctx context.Context) (string, error) {
	var me struct {
		DisplayName  string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
	}
	if err := c.Get(ctx, "/rest/api/3/myself", nil, &me); err != nil {
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrUnreachable) {
			return "", err
		}
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err) // not Jira, or not a site
	}
	return me.DisplayName, nil
}

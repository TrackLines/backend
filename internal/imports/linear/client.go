// Package linear reads what a Linear import needs (TL-473/654): its GraphQL API, as the person whose
// personal API key the org connected.
package linear

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tracklines/backend/internal/httpx"
)

var (
	ErrUnauthorized = httpx.Code("linear_unauthorized", "Linear refused that API key")
	ErrUnreachable  = httpx.Code("linear_unreachable", "couldn't reach Linear")
)

// API is Linear's GraphQL endpoint.
const API = "https://api.linear.app/graphql"

// Client is Linear's API as one person (their personal API key).
type Client struct {
	Key     string
	HTTP    *http.Client
	URL     string              // default API (tests: a fake)
	Uploads string              // default Uploads (tests: a fake)
	sleep   func(time.Duration) // waiting out a rate limit (tests: no wait)
}

const tries = 5

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// Query runs a GraphQL query into out (its data). Rate limits (429, or 400 RATELIMITED) are retried
// after 1s, 2s, 4s…; a refused key is ErrUnauthorized.
func (c Client) Query(ctx context.Context, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
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
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cmp.Or(c.URL, API), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", c.Key) // a personal API key goes as it is, without "Bearer"
		res, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		var reply struct {
			Data   json.RawMessage `json:"data"`
			Errors []gqlError      `json:"errors"`
		}
		_ = json.Unmarshal(raw, &reply)
		code := ""
		if len(reply.Errors) > 0 {
			code = reply.Errors[0].Extensions.Code
		}
		switch {
		case res.StatusCode == http.StatusTooManyRequests || code == "RATELIMITED":
			if try == tries {
				return fmt.Errorf("linear: still rate limited after %d tries", tries)
			}
			sleep(time.Duration(1<<(try-1)) * time.Second)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || code == "AUTHENTICATION_ERROR":
			return ErrUnauthorized
		case len(reply.Errors) > 0:
			return fmt.Errorf("linear: %s", reply.Errors[0].Message)
		case res.StatusCode >= 300:
			return fmt.Errorf("linear: %s", res.Status)
		}
		return json.Unmarshal(reply.Data, out)
	}
}

// connection is one page of a Linear list.
type connection struct {
	Nodes    []json.RawMessage `json:"nodes"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

// Pages calls each with every node of the list at path in query's data ("teams", or "team", "issues"),
// following the cursor: query takes $after (and any vars).
func (c Client) Pages(ctx context.Context, query string, vars map[string]any, path []string, each func(json.RawMessage) error) error {
	v := map[string]any{}
	for k, x := range vars {
		v[k] = x
	}
	for {
		var data map[string]json.RawMessage
		if err := c.Query(ctx, query, v, &data); err != nil {
			return err
		}
		raw := json.RawMessage(nil)
		cur := data
		for i, p := range path {
			raw = cur[p]
			if i < len(path)-1 {
				cur = map[string]json.RawMessage{}
				if err := json.Unmarshal(raw, &cur); err != nil {
					return fmt.Errorf("linear: %s: %w", strings.Join(path, "."), err)
				}
			}
		}
		var page connection
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("linear: %s: %w", strings.Join(path, "."), err)
		}
		for _, n := range page.Nodes {
			if err := each(n); err != nil {
				return err
			}
		}
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" || len(page.Nodes) == 0 {
			return nil
		}
		v["after"] = page.PageInfo.EndCursor
	}
}

// Viewer is who the key belongs to and their workspace: checks the connection works, and names it.
func (c Client) Viewer(ctx context.Context) (name, workspace string, err error) {
	var d struct {
		Viewer struct {
			Name         string `json:"name"`
			Organization struct {
				Name string `json:"name"`
			} `json:"organization"`
		} `json:"viewer"`
	}
	if err := c.Query(ctx, `query { viewer { name organization { name } } }`, nil, &d); err != nil {
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrUnreachable) {
			return "", "", err
		}
		return "", "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return d.Viewer.Name, d.Viewer.Organization.Name, nil
}

// Uploads is where Linear keeps files people attached (in descriptions and comments); they need the key.
const Uploads = "https://uploads.linear.app/"

// Download reads an uploaded file, at most max bytes; only from Linear's uploads, so the key goes nowhere else.
func (c Client) Download(ctx context.Context, u string, max int64) ([]byte, error) {
	if !strings.HasPrefix(u, cmp.Or(c.Uploads, Uploads)) {
		return nil, fmt.Errorf("linear: %s isn't a Linear upload", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.Key)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear: %s: %s", u, res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err == nil && int64(len(data)) > max {
		err = fmt.Errorf("linear: %s is over %d bytes", u, max)
	}
	return data, err
}

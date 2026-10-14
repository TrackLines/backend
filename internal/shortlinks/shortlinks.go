// Package shortlinks gives Tracklines links short ones, through the 1tn shortener (TL-277).
package shortlinks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/httpx"
)

// System shortens links to our own site only (Sites: its origins, e.g. https://tracklin.es), so it
// can't be used to hide anyone else's link behind ours.
type System struct {
	DB     *pgxpool.Pool
	API    string // the shortener's API, e.g. https://api.1tn.pw
	Base   string // where short links live, e.g. https://1tn.pw
	Sites  []string
	Client *http.Client
}

var ErrNotOurs = errors.New("only Tracklines links can be shortened")

// Create: POST /api/short-links {"url": "https://tracklin.es/tickets/<id>"} → {"url": "https://1tn.pw/abc123"}.
func (s System) Create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	short, err := s.Shorten(r.Context(), in.URL)
	if errors.Is(err, ErrNotOurs) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		logs.Errorf("shortlinks: %v", err)
		http.Error(w, "couldn't shorten the link", http.StatusBadGateway)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"url": short})
}

// Shorten returns long's short link, making one the first time.
func (s System) Shorten(ctx context.Context, long string) (string, error) {
	u, err := url.Parse(long)
	if err != nil || u.User != nil || u.Host == "" || !slices.Contains(s.Sites, u.Scheme+"://"+u.Host) || !strings.HasPrefix(u.Path, "/") {
		return "", ErrNotOurs
	}
	var short string
	err = s.DB.QueryRow(ctx, `SELECT short FROM short_links WHERE url = $1`, long).Scan(&short)
	if err == nil {
		return s.link(short), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if short, err = s.create(ctx, long); err != nil {
		return "", err
	}
	// two at once: keep whichever was saved first
	err = s.DB.QueryRow(ctx, `INSERT INTO short_links (url, short) VALUES ($1, $2)
		ON CONFLICT (url) DO UPDATE SET url = EXCLUDED.url RETURNING short`, long, short).Scan(&short)
	return s.link(short), err
}

func (s System) link(short string) string { return strings.TrimRight(s.Base, "/") + "/" + short }

// create asks the shortener; it answers {"short": "yJjWqM"}, or {"status": "<error>"} (still a 200).
func (s System) create(ctx context.Context, long string) (string, error) {
	body, _ := json.Marshal(map[string]string{"url": long})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.API, "/")+"/create", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Short  string `json:"short"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Short == "" {
		return "", fmt.Errorf("shortener answered %d %q: %v", resp.StatusCode, out.Status, err)
	}
	return out.Short, nil
}

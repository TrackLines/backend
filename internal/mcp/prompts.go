package mcp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// MaxPrompt is the longest pre-prompt staff can publish (the table's limit too).
const MaxPrompt = 20000

var (
	ErrPromptEmpty     = httpx.Code("mcp_prompt_empty", "the pre-prompt can't be empty")
	ErrPromptTooLong   = httpx.Code("mcp_prompt_too_long", "the pre-prompt can be up to 20000 characters")
	ErrPromptUnchanged = httpx.Code("mcp_prompt_unchanged", "that's the current version: change the text to publish a new one")
)

// PromptVersion is one published version of the pre-prompt (TL-674). Versions are never changed.
type PromptVersion struct {
	Version     int    `json:"version"`
	Body        string `json:"body,omitempty"` // left out of the version list
	Note        string `json:"note"`           // what changed
	PublishedAt string `json:"published_at"`
	PublishedBy string `json:"published_by,omitempty"` // staff only
}

// PromptStore holds the versioned pre-prompt: what the MCP server tells agents when they connect.
type PromptStore struct {
	DB  *pgxpool.Pool
	TTL time.Duration // how long a session's pre-prompt is reused before the latest is read again; 0: a minute

	mu     *sync.Mutex
	cached *PromptVersion
	at     time.Time
}

func NewPromptStore(db *pgxpool.Pool) *PromptStore { return &PromptStore{DB: db, mu: &sync.Mutex{}} }

const promptCols = `version, body, note, to_char(published_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), published_by`

func scanPrompt(row pgx.Row) (PromptVersion, error) {
	var p PromptVersion
	err := row.Scan(&p.Version, &p.Body, &p.Note, &p.PublishedAt, &p.PublishedBy)
	return p, err
}

// latest reads the newest version, reusing it for TTL.
func (s *PromptStore) latest(ctx context.Context) (PromptVersion, error) {
	if s == nil || s.DB == nil {
		return PromptVersion{}, errors.New("no prompt store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl := s.TTL
	if ttl == 0 {
		ttl = time.Minute
	}
	if s.cached != nil && time.Since(s.at) < ttl {
		return *s.cached, nil
	}
	p, err := scanPrompt(s.DB.QueryRow(ctx, `SELECT `+promptCols+` FROM mcp_prompts ORDER BY version DESC LIMIT 1`))
	if err != nil {
		return PromptVersion{}, err
	}
	s.cached, s.at = &p, time.Now()
	return p, nil
}

// Instructions is the pre-prompt for a new MCP session: the latest version, or the built-in text when it
// can't be read (an agent is never left without instructions).
func (s *PromptStore) Instructions(ctx context.Context) string {
	p, err := s.latest(ctx)
	if err != nil {
		if s != nil && s.DB != nil {
			logs.Errorf("mcp: pre-prompt: %v", err)
		}
		return Instructions
	}
	return p.Body
}

// Publish adds a new version by staff member who; the text must differ from the current one.
func (s *PromptStore) Publish(ctx context.Context, who, body, note string) (PromptVersion, error) {
	body = strings.TrimSpace(body)
	note = strings.Join(strings.Fields(note), " ")
	switch {
	case body == "":
		return PromptVersion{}, ErrPromptEmpty
	case len([]rune(body)) > MaxPrompt:
		return PromptVersion{}, ErrPromptTooLong
	}
	if len([]rune(note)) > 500 {
		note = string([]rune(note)[:500])
	}
	var p PromptVersion
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE mcp_prompts IN SHARE ROW EXCLUSIVE MODE`); err != nil { // one publish at a time
			return err
		}
		var current string
		if err := tx.QueryRow(ctx, `SELECT body FROM mcp_prompts ORDER BY version DESC LIMIT 1`).Scan(&current); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current == body {
			return ErrPromptUnchanged
		}
		var err error
		p, err = scanPrompt(tx.QueryRow(ctx, `INSERT INTO mcp_prompts (version, body, note, published_by)
			SELECT COALESCE(max(version), 0) + 1, $1, $2, $3 FROM mcp_prompts RETURNING `+promptCols, body, note, who))
		return err
	})
	if err != nil {
		return PromptVersion{}, err
	}
	s.mu.Lock()
	s.cached = nil // new sessions get it straight away on this instance
	s.mu.Unlock()
	return p, nil
}

func (s *PromptStore) versions(ctx context.Context) ([]PromptVersion, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+promptCols+` FROM mcp_prompts ORDER BY version DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PromptVersion, error) { return scanPrompt(r) })
}

// Prompts: GET /api/mcp/prompts, public. The latest pre-prompt (with its version and date) and the tool
// descriptions agents get, from the same values the server sends, so the docs can't drift (TL-565, TL-674).
func (s *PromptStore) Prompts(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Instructions string   `json:"instructions"`
		Version      int      `json:"version,omitempty"`
		PublishedAt  string   `json:"published_at,omitempty"`
		Tools        []Prompt `json:"tools"`
	}{Instructions: Instructions}
	if p, err := s.latest(r.Context()); err == nil {
		out.Instructions, out.Version, out.PublishedAt = p.Body, p.Version, p.PublishedAt
	}
	for _, t := range tools {
		out.Tools = append(out.Tools, Prompt{t.name, t.title, t.description})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Versions: GET /api/mcp/prompts/versions, public: every version, newest first, without their text.
func (s *PromptStore) Versions(w http.ResponseWriter, r *http.Request) {
	list, err := s.versions(r.Context())
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	for i := range list {
		list[i].Body, list[i].PublishedBy = "", ""
	}
	httpx.JSON(w, http.StatusOK, list)
}

// Version: GET /api/mcp/prompts/versions/{version}, public: one version as it was published.
func (s *PromptStore) Version(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || n < 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p, err := scanPrompt(s.DB.QueryRow(r.Context(), `SELECT `+promptCols+` FROM mcp_prompts WHERE version = $1`, n))
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	p.PublishedBy = ""
	httpx.JSON(w, http.StatusOK, p)
}

// Staff: GET /api/staff/mcp-prompt: the current version and every version, with who published each.
func (s *PromptStore) Staff(w http.ResponseWriter, r *http.Request) {
	list, err := s.versions(r.Context())
	if err != nil {
		httpx.Error(w, r, http.StatusInternalServerError, err)
		return
	}
	out := struct {
		Current  *PromptVersion  `json:"current"`
		Versions []PromptVersion `json:"versions"`
	}{Versions: list}
	if len(list) > 0 {
		out.Current = &list[0]
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Publish: POST /api/staff/mcp-prompt {body, note}: a new version, used by new MCP sessions from now on.
func (s *PromptStore) PublishHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body string `json:"body"`
		Note string `json:"note"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	who, _ := auth.UserID(r.Context())
	p, err := s.Publish(r.Context(), who, in.Body, in.Note)
	switch {
	case errors.Is(err, ErrPromptEmpty), errors.Is(err, ErrPromptTooLong):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrPromptUnchanged):
		httpx.Error(w, r, http.StatusConflict, err)
	case err != nil:
		httpx.Error(w, r, http.StatusInternalServerError, err)
	default:
		httpx.JSON(w, http.StatusCreated, p)
	}
}

// Package help is the help panel's answers (TL-575): in the database so staff change them in the admin
// panel without a deploy. Everyone signed in reads the published ones in their language; the .po export
// is what the TUI bakes in.
package help

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/users"
)

// Pages are the kinds of page an answer can be suggested on, before anything's typed.
var Pages = []string{"project", "board", "dashboard", "settings", "account"}

// Answer is one answer with every translation, as staff edit it.
type Answer struct {
	ID           string            `json:"id"`
	Slug         string            `json:"slug"`
	Question     map[string]string `json:"question"` // by locale; en required
	Answer       map[string]string `json:"answer"`
	Href         string            `json:"href"` // an app route; [project] and [board] are filled in from the page
	FallbackHref string            `json:"fallback_href"`
	Pages        []string          `json:"pages"`
	Words        string            `json:"words"` // extra English search terms
	Position     int               `json:"position"`
	Published    bool              `json:"published"`
	UpdatedBy    string            `json:"updated_by"`
	UpdatedAt    string            `json:"updated_at"`
}

// Localized is a published answer in one language, as the help panel shows it.
type Localized struct {
	Slug         string   `json:"slug"`
	Question     string   `json:"question"`
	Answer       string   `json:"answer"`
	Href         string   `json:"href"`
	FallbackHref string   `json:"fallback_href"`
	Pages        []string `json:"pages"`
	Words        string   `json:"words"`
}

var (
	ErrNotFound    = httpx.Code("help_not_found", "help answer not found")
	ErrSlug        = httpx.Code("help_slug", "the slug is lower-case letters, digits and dashes")
	ErrSlugTaken   = httpx.Code("help_slug_taken", "another answer has that slug")
	ErrEnglish     = httpx.Code("help_english", "the English question and answer are required")
	ErrLocale      = httpx.Code("help_locale", "unknown language")
	ErrHref        = httpx.Code("help_href", "a link must be an app route, starting with /")
	ErrPage        = httpx.Code("help_page", "unknown page kind")
	ErrOrder       = httpx.Code("help_order", "send every answer's id, once each")
	ErrFetchFailed = httpx.Code("help_failed", "could not load help answers")
)

type System struct{ DB *pgxpool.Pool }

const columns = `id::text, slug, question, answer, href, fallback_href, pages, words, position, published, updated_by, to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

func (s System) all(r *http.Request, publishedOnly bool) ([]Answer, error) {
	where := ""
	if publishedOnly {
		where = " WHERE published"
	}
	rows, err := s.DB.Query(r.Context(), `SELECT `+columns+` FROM help_answers`+where+` ORDER BY position, slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Answer, error) {
		var a Answer
		return a, row.Scan(&a.ID, &a.Slug, &a.Question, &a.Answer, &a.Href, &a.FallbackHref, &a.Pages, &a.Words, &a.Position, &a.Published, &a.UpdatedBy, &a.UpdatedAt)
	})
}

// in is text in locale, else English.
func in(t map[string]string, locale string) string {
	if v := t[locale]; v != "" {
		return v
	}
	return t["en"]
}

func locale(r *http.Request) string {
	if l := r.URL.Query().Get("locale"); slices.Contains(users.Languages, l) {
		return l
	}
	return "en"
}

func (s System) failed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	logs.Errorf("help: %v", err)
	httpx.Error(w, r, http.StatusInternalServerError, ErrFetchFailed)
}

// List answers GET /api/help?locale=nl: the published answers in that language (English where there's
// no translation). The ETag is the body's hash, so a client that has them gets a 304.
func (s System) List(w http.ResponseWriter, r *http.Request) {
	all, err := s.all(r, true)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	l := locale(r)
	out := make([]Localized, 0, len(all))
	for _, a := range all {
		out = append(out, Localized{Slug: a.Slug, Question: in(a.Question, l), Answer: in(a.Answer, l), Href: a.Href, FallbackHref: a.FallbackHref, Pages: a.Pages, Words: a.Words})
	}
	body, _ := json.Marshal(out)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// Export answers GET /api/help/export.po?locale=nl: the published answers as gettext, for the TUI to bake
// in. Each answer is two messages, msgctxt "<slug>:question" and "<slug>:answer", msgid the English;
// msgstr is "" where there's no translation (gettext then shows the English). Comments carry the rest.
func (s System) Export(w http.ResponseWriter, r *http.Request) {
	all, err := s.all(r, true)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	l := locale(r)
	var b strings.Builder
	fmt.Fprintf(&b, "msgid \"\"\nmsgstr \"\"\n\"Content-Type: text/plain; charset=UTF-8\\n\"\n\"Language: %s\\n\"\n", l)
	for _, a := range all {
		for _, part := range []struct {
			name string
			text map[string]string
		}{{"question", a.Question}, {"answer", a.Answer}} {
			b.WriteString("\n")
			if part.name == "question" {
				fmt.Fprintf(&b, "#. href: %s\n#. fallback_href: %s\n#. pages: %s\n#. words: %s\n", a.Href, a.FallbackHref, strings.Join(a.Pages, " "), a.Words)
			}
			fmt.Fprintf(&b, "msgctxt %s\nmsgid %s\nmsgstr %s\n", poString(a.Slug+":"+part.name), poString(part.text["en"]), poString(part.text[l]))
		}
	}
	w.Header().Set("Content-Type", "text/x-gettext-translation; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="help.%s.po"`, l))
	_, _ = w.Write([]byte(b.String()))
}

func poString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}

// Staff answers GET /api/staff/help: every answer, unpublished too, with every translation.
func (s System) Staff(w http.ResponseWriter, r *http.Request) {
	all, err := s.all(r, false)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, all)
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (a *Answer) check() error {
	a.Slug = strings.TrimSpace(a.Slug)
	if !slugRE.MatchString(a.Slug) {
		return ErrSlug
	}
	for _, t := range []map[string]string{a.Question, a.Answer} {
		for l, v := range t {
			if !slices.Contains(users.Languages, l) {
				return ErrLocale
			}
			if t[l] = strings.TrimSpace(v); t[l] == "" {
				delete(t, l)
			}
		}
	}
	if a.Question["en"] == "" || a.Answer["en"] == "" {
		return ErrEnglish
	}
	for _, h := range []string{a.Href, a.FallbackHref} {
		if h != "" && (!strings.HasPrefix(h, "/") || strings.HasPrefix(h, "//") || strings.Contains(h, `\`)) { // browsers read /\x as //x
			return ErrHref
		}
	}
	if a.Pages == nil {
		a.Pages = []string{}
	}
	for _, p := range a.Pages {
		if !slices.Contains(Pages, p) {
			return ErrPage
		}
	}
	return nil
}

func (s System) read(w http.ResponseWriter, r *http.Request) (Answer, bool) {
	var a Answer
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&a); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid json"))
		return a, false
	}
	if a.Question == nil {
		a.Question = map[string]string{}
	}
	if a.Answer == nil {
		a.Answer = map[string]string{}
	}
	if err := a.check(); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, err)
		return a, false
	}
	return a, true
}

func (s System) saved(w http.ResponseWriter, r *http.Request, status int, row pgx.Row) {
	var a Answer
	err := row.Scan(&a.ID, &a.Slug, &a.Question, &a.Answer, &a.Href, &a.FallbackHref, &a.Pages, &a.Words, &a.Position, &a.Published, &a.UpdatedBy, &a.UpdatedAt)
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, r, http.StatusNotFound, ErrNotFound)
	case errors.As(err, &pg) && pg.Code == "23505":
		httpx.Error(w, r, http.StatusConflict, ErrSlugTaken)
	case err != nil:
		s.failed(w, r, err)
	default:
		httpx.JSON(w, status, a)
	}
}

// Create answers POST /api/staff/help: a new answer, last.
func (s System) Create(w http.ResponseWriter, r *http.Request) {
	a, ok := s.read(w, r)
	if !ok {
		return
	}
	s.saved(w, r, http.StatusCreated, s.DB.QueryRow(r.Context(), `INSERT INTO help_answers (slug, question, answer, href, fallback_href, pages, words, published, updated_by, position)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, (SELECT COALESCE(max(position) + 1, 0) FROM help_answers)) RETURNING `+columns,
		a.Slug, a.Question, a.Answer, a.Href, a.FallbackHref, a.Pages, a.Words, a.Published, auth.ActorID(r.Context())))
}

// Update answers PUT /api/staff/help/{id}: replaces the answer (its place stays).
func (s System) Update(w http.ResponseWriter, r *http.Request) {
	a, ok := s.read(w, r)
	if !ok {
		return
	}
	s.saved(w, r, http.StatusOK, s.DB.QueryRow(r.Context(), `UPDATE help_answers SET slug = $2, question = $3, answer = $4, href = $5, fallback_href = $6,
		pages = $7, words = $8, published = $9, updated_by = $10, updated_at = now() WHERE id::text = $1 RETURNING `+columns,
		r.PathValue("id"), a.Slug, a.Question, a.Answer, a.Href, a.FallbackHref, a.Pages, a.Words, a.Published, auth.ActorID(r.Context())))
}

// Delete answers DELETE /api/staff/help/{id}.
func (s System) Delete(w http.ResponseWriter, r *http.Request) {
	tag, err := s.DB.Exec(r.Context(), `DELETE FROM help_answers WHERE id::text = $1`, r.PathValue("id"))
	if err != nil {
		s.failed(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, r, http.StatusNotFound, ErrNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Order answers PUT /api/staff/help/order {"ids": [...]}: every answer's id, in the new order.
func (s System) Order(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid json"))
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		s.failed(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	tag, err := tx.Exec(r.Context(), `UPDATE help_answers h SET position = o.n - 1, updated_at = now()
		FROM unnest($1::text[]) WITH ORDINALITY AS o(id, n) WHERE h.id::text = o.id`, body.IDs)
	if err != nil {
		s.failed(w, r, err)
		return
	}
	var total, distinct int
	if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM help_answers), (SELECT count(DISTINCT x) FROM unnest($1::text[]) x)`, body.IDs).Scan(&total, &distinct); err != nil {
		s.failed(w, r, err)
		return
	}
	// every answer, once each, or nothing changes
	if n := len(body.IDs); n == 0 || distinct != n || total != n || int(tag.RowsAffected()) != n {
		httpx.Error(w, r, http.StatusBadRequest, ErrOrder)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.failed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

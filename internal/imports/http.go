package imports

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/httpx"
)

// admins is who may import: org admins, signed in (never API keys).
type admins interface {
	RequireAdmin(w http.ResponseWriter, r *http.Request) (org, user string, ok bool)
}

type System struct {
	store   Store
	admins  admins
	sources map[string]Source
	on      func() bool  // the "imports" flag: off, every route is a 404
	spawn   func(func()) // runs an import in the background (tests: right away)
}

// NewSystem exposes the import handlers for sources by name ("jira"); routes are in internal/service.go.
func NewSystem(db *pgxpool.Pool, a admins, on func() bool, sources map[string]Source) System {
	return System{store: Store{DB: db}, admins: a, sources: sources, on: on, spawn: func(f func()) { go f() }}
}

// WithFiles lets imports copy attachments to f (without it they're linked instead).
func (h System) WithFiles(f Files) System {
	h.store.Files = f
	return h
}

// gate: the flag, then org admins only.
func (h System) gate(w http.ResponseWriter, r *http.Request) (org, user string, ok bool) {
	if h.on == nil || !h.on() {
		http.NotFound(w, r)
		return "", "", false
	}
	return h.admins.RequireAdmin(w, r)
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case httpx.Canceled(err):
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrProblems), errors.Is(err, ErrNotPreview), errors.Is(err, ErrNotRunning), errors.Is(err, ErrNotDone), errors.Is(err, ErrChanged):
		httpx.Error(w, r, http.StatusConflict, err)
	default:
		logs.Errorf("imports: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

var errUnknownSource = httpx.Code("unknown_import_source", "unknown import source")

// Preview: POST /api/imports {"source": "jira", "options": {…}}. The source works out the plan; the
// preview (201) says what it creates and every problem (plan limits, prefixes) to fix before it can run.
func (h System) Preview(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.gate(w, r)
	if !ok {
		return
	}
	var in struct {
		Source  string          `json:"source"`
		Options json.RawMessage `json:"options"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	src, found := h.sources[in.Source]
	if !found {
		httpx.Error(w, r, http.StatusBadRequest, errUnknownSource)
		return
	}
	plan, err := src.Plan(r.Context(), org, in.Options)
	if err != nil {
		httpx.Error(w, r, http.StatusBadRequest, err) // the source says what's wrong (credentials, project…)
		return
	}
	im, err := h.store.Create(r.Context(), org, user, in.Source, plan)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, im)
}

// List: GET /api/imports, the org's imports, newest first.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	out, err := h.store.List(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Get: GET /api/imports/{id}: a preview, or a run's progress and report.
func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	im, err := h.store.Get(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, im)
}

// Run: POST /api/imports/{id}/run confirms a preview without problems; it runs in the background (202).
func (h System) Run(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.gate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	plan, err := h.store.Start(r.Context(), org, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	im, err := h.store.Get(r.Context(), org, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	h.spawn(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Hour)
		defer cancel()
		fetch, _ := h.sources[im.Source].(Fetcher)
		h.store.Run(ctx, org, user, id, im.Source, plan, fetch)
	})
	httpx.JSON(w, http.StatusAccepted, im)
}

// Cancel: POST /api/imports/{id}/cancel drops a preview, or stops a running import (nothing is kept).
func (h System) Cancel(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	if err := h.store.Cancel(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Scrap: POST /api/imports/{id}/scrap deletes everything a finished import created, while nobody has
// acted on it (409 import_changed once anyone has).
func (h System) Scrap(w http.ResponseWriter, r *http.Request) {
	org, _, ok := h.gate(w, r)
	if !ok {
		return
	}
	if err := h.store.Scrap(r.Context(), org, r.PathValue("id")); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

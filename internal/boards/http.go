package boards

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

// NewSystem exposes the board handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool, admins organizations.Admins) System {
	return System{store: Store{DB: db}, admins: admins}
}

type System struct {
	store  Store
	admins organizations.Admins
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		logs.Errorf("boards: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Create adds a board to project {id} (one board per team).
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		TemplateID  string `json:"template_id"` // optional: a built-in or org template; default Simple
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	org := auth.OrgID(r.Context())
	tpl := Builtins[0]
	if in.TemplateID != "" {
		var err error
		if tpl, err = h.store.Template(r.Context(), org, in.TemplateID); err != nil {
			http.Error(w, "template not found", http.StatusBadRequest)
			return
		}
	}
	b, err := h.store.CreateFrom(r.Context(), org, r.PathValue("id"), in.Name, in.Description, tpl)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, b)
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	b, err := h.store.Get(r.Context(), r.PathValue("id"), org)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

// Update changes the board name and estimate scale. Switching scale clears open estimates.
func (h System) Update(w http.ResponseWriter, r *http.Request) {
	if !h.admins.RequireResourceBoardManager(w, r, "board", r.PathValue("id")) {
		return
	}
	var in struct {
		Name          *string `json:"name"`
		EstimateScale *string `json:"estimate_scale"`
		Style         *string `json:"style"` // sprints | kanban
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name == nil && in.EstimateScale == nil && in.Style == nil {
		http.Error(w, "name, estimate_scale or style is required", http.StatusBadRequest)
		return
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
	}
	if in.EstimateScale != nil {
		if _, ok := Scales[*in.EstimateScale]; !ok {
			http.Error(w, "estimate_scale must be none, fibonacci, tshirt, powers or linear", http.StatusBadRequest)
			return
		}
	}
	if in.Style != nil && *in.Style != "sprints" && *in.Style != "kanban" {
		http.Error(w, "style must be sprints or kanban", http.StatusBadRequest)
		return
	}
	if in.Style != nil {
		if err := h.store.SetStyle(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), *in.Style); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.Name != nil || in.EstimateScale != nil {
		if err := h.store.UpdateSettings(r.Context(), r.PathValue("id"), auth.OrgID(r.Context()), name, in.EstimateScale); err != nil {
			writeErr(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.admins.RequireAdmin(w, r); !ok {
		return
	}
	org := auth.OrgID(r.Context())
	if err := h.store.Delete(r.Context(), r.PathValue("id"), org); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

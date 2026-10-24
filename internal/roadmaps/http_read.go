package roadmaps

import (
	"context"
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// NewSystem exposes the roadmap handlers; routes are declared in internal/service.go. Without
// WithManagers nobody can change a roadmap.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

// Managers says who may change a project's roadmaps: org admins and its teams' leaders (TL-413).
// Publishing one (public, or no longer public) and deleting one are for org admins only (TL-432).
type Managers interface {
	IsProjectManager(ctx context.Context, org, user, project string) (bool, error)
	IsAdmin(ctx context.Context, org, user string) (bool, error)
}

type System struct {
	store    Store
	managers Managers
	people   func(ctx context.Context, org string) ([]notifications.Person, error) // names updates' authors
}

func (h System) WithManagers(m Managers) System { h.managers = m; return h }

func (h System) WithPeople(p func(ctx context.Context, org string) ([]notifications.Person, error)) System {
	h.people = p
	return h
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrInvalidVisibility), errors.Is(err, ErrOtherProject), errors.Is(err, ErrBadDates), errors.Is(err, ErrInvalidItemStatus), errors.Is(err, ErrInvalidUpdate):
		httpx.Error(w, r, http.StatusBadRequest, err)
	default:
		logs.Errorf("roadmaps: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// manager lets only org admins and the project's team leaders change its roadmaps (TL-413), and
// reports whether they can. project: known for Create; otherwise roadmap's project is looked up (in
// the caller's organization, so another org's roadmap is not found). An API key counts as its owner,
// so a leader's agent can keep its roadmaps up to date.
func (h System) manager(w http.ResponseWriter, r *http.Request, roadmap, project string) bool {
	ctx := r.Context()
	org := auth.OrgID(ctx)
	user, ok := auth.UserID(ctx)
	if !ok || org == "" {
		httpx.Error(w, r, http.StatusForbidden, httpx.ErrSignIn)
		return false
	}
	if project == "" {
		rm, err := h.store.Get(ctx, roadmap)
		if err != nil {
			writeErr(w, r, err)
			return false
		}
		if rm.OwnerClerkID != org {
			http.Error(w, "not found", http.StatusNotFound)
			return false
		}
		project = rm.ProjectID
	}
	can, err := h.canManage(ctx, org, user, project)
	if err != nil {
		writeErr(w, r, err)
		return false
	}
	if !can {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("roadmap_admins_or_leaders", "only organization admins and this project's team leaders can change its roadmaps"))
		return false
	}
	return true
}

// errPublisher: only a signed-in org admin publishes, unpublishes or deletes a roadmap.
var errPublisher = httpx.Code("roadmap_publish_admins", "only an organization admin, signed in, can publish, unpublish or delete a roadmap")

// publisher lets only a signed-in org admin make a roadmap public, take it off public or delete it,
// never an API key, even an admin's: what the outside world sees is a person's decision (TL-432).
func (h System) publisher(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	if auth.ViaAPIKey(ctx) || h.managers == nil {
		httpx.Error(w, r, http.StatusForbidden, errPublisher)
		return false
	}
	user, _ := auth.UserID(ctx)
	admin, err := h.managers.IsAdmin(ctx, auth.OrgID(ctx), user)
	if err != nil {
		writeErr(w, r, err)
		return false
	}
	if !admin {
		httpx.Error(w, r, http.StatusForbidden, errPublisher)
	}
	return admin
}

func (h System) canManage(ctx context.Context, org, user, project string) (bool, error) {
	if h.managers == nil {
		return false, nil
	}
	return h.managers.IsProjectManager(ctx, org, user, project)
}

// canRead: public → anyone; login_only and team → signed-in members of the owning org (org). Other
// organizations never read a login_only roadmap (TL-432). Returns 0 when allowed, else the status to send.
func canRead(r *Roadmap, org string, signedIn bool) int {
	switch {
	case r.Visibility == Public, org != "" && r.OwnerClerkID == org && signedIn:
		return 0
	case !signedIn:
		return http.StatusUnauthorized
	default:
		return http.StatusForbidden
	}
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	rm, err := h.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	_, ok := auth.UserID(r.Context())
	org := auth.OrgID(r.Context())
	if status := canRead(rm, org, ok); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if !ok || rm.OwnerClerkID != org {
		redactTickets(rm) // public/login-only readers see progress counts, never ticket titles or ids
	} else if user, _ := auth.UserID(r.Context()); user != "" {
		if rm.CanManage, err = h.canManage(r.Context(), org, user, rm.ProjectID); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	httpx.JSON(w, http.StatusOK, rm)
}

// list returns the caller's own roadmaps. Public roadmaps are deliberately never listed:
// they're reachable only via their shared /r/<id> link.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListByOwner(r.Context(), auth.OrgID(r.Context()))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// redactTickets drops linked-ticket details, keeping each item's progress counts.
func redactTickets(rm *Roadmap) {
	for i := range rm.Items {
		rm.Items[i].Tickets = nil
	}
}

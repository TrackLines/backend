package roadmaps

import (
	"errors"
	"net/http"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

// NewSystem exposes the roadmap handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{Store{DB: db}}
}

type System struct{ store Store }

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrInvalidVisibility), errors.Is(err, ErrOtherProject), errors.Is(err, ErrBadDates), errors.Is(err, ErrInvalidItemStatus):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		logs.Errorf("roadmaps: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// canRead: public → anyone, login_only → any signed-in user, team → owner only (team members are post-v1).
// Returns 0 when allowed, else the status to send.
func canRead(r *Roadmap, user string, signedIn bool) int {
	switch {
	case r.Visibility == Public, r.OwnerClerkID == user && signedIn:
		return 0
	case !signedIn:
		return http.StatusUnauthorized
	case r.Visibility == LoginOnly:
		return 0
	default:
		return http.StatusForbidden
	}
}

func (h System) Get(w http.ResponseWriter, r *http.Request) {
	rm, err := h.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	user, ok := auth.UserID(r.Context())
	if status := canRead(rm, user, ok); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if !ok || rm.OwnerClerkID != user {
		redactTickets(rm) // public/login-only readers see progress counts, never ticket titles or ids
	}
	httpx.JSON(w, http.StatusOK, rm)
}

// list returns the caller's own roadmaps. Public roadmaps are deliberately never listed:
// they're reachable only via their shared /r/<id> link.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserID(r.Context())
	out, err := h.store.ListByOwner(r.Context(), user)
	if err != nil {
		writeErr(w, err)
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

package attachments

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
)

type System struct {
	store Store
	// IsAdmin: is user an org admin (organizations.Admins.IsAdmin); admins can keep any file (TL-259)
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
	// Files stores uploads sent to the backend (Upload, TL-659); nil when UploadThing isn't set up
	Files Files
}

// Files is UploadThing (retention.UploadThing): upload a file, delete files by key.
type Files interface {
	Upload(ctx context.Context, name, contentType string, data []byte) (key, url string, err error)
	DeleteFiles(ctx context.Context, keys ...string) error
}

// NewSystem exposes the attachment handlers; routes are declared in internal/service.go.
func NewSystem(db *pgxpool.Pool) System {
	return System{store: Store{DB: db}}
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.Canceled(err) {
		return // the client left: nobody to answer, nothing to report
	}
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadFile), errors.Is(err, ErrTooBig), errors.Is(err, ErrBadUpload):
		httpx.Error(w, r, http.StatusBadRequest, err)
	case errors.Is(err, ErrCantKeep), errors.Is(err, ErrCantRemove):
		httpx.Error(w, r, http.StatusForbidden, err)
	case errors.Is(err, ErrTooMany):
		httpx.Error(w, r, http.StatusConflict, err)
	case errors.Is(err, ErrDone):
		httpx.Error(w, r, http.StatusConflict, err)
	default:
		logs.Errorf("attachments: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// List returns ticket {id}'s attachments.
func (h System) List(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	out, err := h.store.List(r.Context(), org, r.PathValue("id"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Create records a file the browser already uploaded to UploadThing: {"key","url","name","size","content_type"}.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in Attachment
	if !httpx.Decode(w, r, &in) {
		return
	}
	org := auth.OrgID(r.Context())
	a, err := h.store.Create(r.Context(), org, auth.ActorID(r.Context()), r.PathValue("id"), in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, a)
}

// Upload: POST /api/tickets/{id}/attachments/upload?name=<file name>, the file as the body and its
// type as Content-Type (TL-659). For API-key clients like the TUI, which can't hold the UploadThing
// secret: the ticket is checked before anything is stored, the file goes up from here, and it's deleted
// again if recording it fails. Read-only keys never get here (apikeys.Middleware refuses a POST).
func (h System) Upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.Files == nil {
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("uploads_unavailable", "file uploads aren't set up on this server"))
		return
	}
	name := strings.TrimSpace(path.Base(strings.ReplaceAll(r.URL.Query().Get("name"), `\`, "/")))
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(contentType); err != nil || name == "" || name == "." || name == ".." || name == "/" {
		writeErr(w, r, ErrBadUpload)
		return
	}
	if r.ContentLength > MaxFileBytes {
		writeErr(w, r, ErrTooBig)
		return
	}
	org := auth.OrgID(ctx)
	ticket := r.PathValue("id")
	if err := h.store.CanAdd(ctx, org, ticket); err != nil {
		writeErr(w, r, err)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxFileBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, r, ErrTooBig)
		} else if ctx.Err() == nil {
			httpx.Error(w, r, http.StatusBadRequest, ErrBadUpload)
		}
		return
	}
	key, link, err := h.Files.Upload(ctx, name, contentType, data)
	if err != nil {
		if ctx.Err() == nil {
			logs.Errorf("attachments: upload: %v", err)
			httpx.Error(w, r, http.StatusBadGateway, httpx.Code("upload_failed", "the file couldn't be stored: please try again"))
		}
		return
	}
	a, err := h.store.Create(ctx, org, auth.ActorID(ctx), ticket, Attachment{Key: key, URL: link, Name: name, Size: int64(len(data)), ContentType: contentType})
	if err != nil {
		// nothing points at the file: delete it now rather than leave it stored (the request may be gone)
		if derr := h.Files.DeleteFiles(context.WithoutCancel(ctx), key); derr != nil {
			logs.Errorf("attachments: delete unrecorded upload %s: %v", key, derr)
		}
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, a)
}

// uploaderOrAdmin: may this caller remove or keep a file uploaded by uploader? Whoever uploaded it
// (a person, or an agent or service key by its name), or an org admin; agent and service keys act
// only for themselves, never with their owner's admin rights (TL-659).
func (h System) uploaderOrAdmin(r *http.Request) func(uploader string) bool {
	return func(uploader string) bool {
		ctx := r.Context()
		if uploader == auth.ActorID(ctx) {
			return true
		}
		user, signedIn := auth.UserID(ctx)
		if !signedIn || h.IsAdmin == nil || (auth.ViaAPIKey(ctx) && auth.APIKeyKind(ctx) != "personal") {
			return false
		}
		admin, _ := h.IsAdmin(ctx, auth.OrgID(ctx), user)
		return admin
	}
}

// Delete removes attachment {id}, for whoever uploaded it or an org admin; its file on UploadThing
// goes with the next sweep (Sweeper), the same way files go when their ticket, project or organization is deleted.
func (h System) Delete(w http.ResponseWriter, r *http.Request) {
	org := auth.OrgID(r.Context())
	if _, err := h.store.Delete(r.Context(), org, r.PathValue("id"), h.uploaderOrAdmin(r)); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Keep: POST /api/attachments/{id}/keep holds on to a file due for clean-up for another year (TL-259).
// People only: an org admin, or whoever uploaded it.
func (h System) Keep(w http.ResponseWriter, r *http.Request) {
	_, signedIn := auth.UserID(r.Context())
	if auth.ViaAPIKey(r.Context()) || !signedIn {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("sign_in_keep_file", "sign in to keep a file"))
		return
	}
	org := auth.OrgID(r.Context())
	err := h.store.Keep(r.Context(), org, r.PathValue("id"), h.uploaderOrAdmin(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Retention: GET /api/attachments/retention is the org's file clean-up (TL-260): files due to be
// deleted and files already deleted. Org admins only, and people only.
func (h System) Retention(w http.ResponseWriter, r *http.Request) {
	user, signedIn := auth.UserID(r.Context())
	org := auth.OrgID(r.Context())
	admin := false
	if signedIn && !auth.ViaAPIKey(r.Context()) && h.IsAdmin != nil {
		admin, _ = h.IsAdmin(r.Context(), org, user)
	}
	if !admin {
		httpx.Error(w, r, http.StatusForbidden, httpx.Code("admins_file_cleanup", "only org admins can see the file clean-up"))
		return
	}
	out, err := h.store.Retention(r.Context(), org)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

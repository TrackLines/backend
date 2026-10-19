package internal

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/tracklines/backend/internal/auth"
)

// Chat buttons (TL-455) call routes in-process as the linked person: signed in (not an API key), in
// the org on the button, with the body passed on, through the same auth middleware as the web app.
func TestActAs(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("POST /api/tickets/{id}/comments", auth.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.UserID(r.Context())
		body, _ := io.ReadAll(r.Body)
		if auth.ViaAPIKey(r.Context()) {
			http.Error(w, "api key", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(user + " " + auth.OrgID(r.Context()) + " " + auth.ActorID(r.Context()) + " " + r.PathValue("id") + " " + string(body)))
	})))
	status, out := actAs(mux)(context.Background(), "user_ava", "org_x", http.MethodPost, "/api/tickets/t1/comments", map[string]string{"body": "On it"})
	if status != http.StatusOK || string(out) != "user_ava org_x user_ava t1 {\"body\":\"On it\"}\n" {
		t.Fatalf("%d %q", status, out)
	}
}

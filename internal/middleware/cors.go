package middleware

import (
	"net/http"

	bm "github.com/bugfixes/go-bugfixes/middleware"
)

// CORS returns a Bugfixes CORS middleware configured with the given origins.
func CORS(origins []string) func(http.Handler) http.Handler {
	s := bm.NewMiddleware()
	for _, o := range origins {
		s.AddAllowedOrigins(o)
	}
	s.AddAllowedMethods(http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions)
	s.AddAllowedHeaders("Content-Type", "Authorization", "X-Requested-With")
	return s.CORS
}

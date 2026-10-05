package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	bm "github.com/bugfixes/go-bugfixes/middleware"
	"github.com/tracklines/backend/internal/auth"
)

// Wrap returns middleware that recovers from panics, assigns request IDs,
// logs requests, and attaches the clerk user id to the request context
// when a valid session is present.
func Wrap(next http.Handler) http.Handler {
	return bm.Recoverer(
		bm.RequestID(
			requestLogger(
				attachUserID(next),
			),
		),
	)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		status := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(status, r)
		if status.code == 0 {
			status.code = http.StatusOK
		}
		requestID, _ := r.Context().Value(bm.RequestIDKey).(string)
		userID, _ := r.Context().Value(ctxKey{}).(string)
		logs.Infof("http request method=%s path=%s status=%d duration=%s request_id=%s user_id=%s",
			r.Method, r.URL.Path, status.code, time.Since(started), requestID, userID)
	})
}

// statusWriter preserves the ResponseController unwrap path while tracking
// the status for BugFixes request reports.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code != 0 {
		return
	}
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func attachUserID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uid, ok := auth.UserID(r.Context()); ok {
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, uid))
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

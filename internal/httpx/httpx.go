// Package httpx holds the JSON helpers every domain handler uses.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Decode reads a JSON body capped at 1MB; writes 400 and returns false on failure.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		Error(w, r, http.StatusBadRequest, errors.New("invalid json"))
		return false
	}
	return true
}

// Canceled: the request was given up on (the person left the page, or the client timed out), so
// there's nobody to answer and nothing worth reporting to Bugfixes.
func Canceled(err error) bool { return errors.Is(err, context.Canceled) }

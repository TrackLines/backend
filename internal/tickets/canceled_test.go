package tickets

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
)

// TL-414/415: a request the browser gave up on isn't answered or reported as a server error.
func TestWriteErrIgnoresCanceled(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, fmt.Errorf("list: %w", context.Canceled))
	if rec.Body.Len() != 0 || rec.Code != 200 { // untouched: nothing written
		t.Errorf("cancelled: %d %q", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	writeErr(rec, fmt.Errorf("boom"))
	if rec.Code != 500 {
		t.Errorf("a real failure: %d", rec.Code)
	}
}

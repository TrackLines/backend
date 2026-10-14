package comments

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// a comment over the bound is refused before anything is stored (TL-251)
func TestCommentBound(t *testing.T) {
	rec := httptest.NewRecorder()
	System{}.Create(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"body":"`+strings.Repeat("x", maxBody+1)+`"}`)))
	if rec.Code != 400 {
		t.Fatalf("over-long comment: %d", rec.Code)
	}
}

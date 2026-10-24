package tickets

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// tickets have bounds (TL-251): a title fits a card, a description stays searchable, and a huge page
// number is refused instead of overflowing into a 500
func TestTicketBounds(t *testing.T) {
	ok := func(in ticketInput) bool {
		return in.valid(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	}
	if !ok(ticketInput{Title: strings.Repeat("é", MaxTitle), Description: strings.Repeat("x", MaxBody)}) {
		t.Fatal("the limits themselves should be allowed")
	}
	if ok(ticketInput{Title: strings.Repeat("a", MaxTitle+1)}) {
		t.Fatal("over-long title accepted")
	}
	if ok(ticketInput{Title: "t", Description: strings.Repeat("x", MaxBody+1)}) {
		t.Fatal("over-long description accepted")
	}
	rec := httptest.NewRecorder()
	if _, fine := intParam(rec, httptest.NewRequest("GET", "/", nil), "922337203685477580", "page", 1, 100_000); fine || rec.Code != 400 {
		t.Fatalf("huge page: %d", rec.Code)
	}
}

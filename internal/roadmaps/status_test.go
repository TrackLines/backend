package roadmaps

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeriveItemStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manual   string
		progress Progress
		want     string
	}{
		{"manual not started without links", StatusNotStarted, Progress{}, StatusNotStarted},
		{"manual in progress without links", StatusInProgress, Progress{}, StatusInProgress},
		{"manual done without links", StatusDone, Progress{}, StatusDone},
		{"defaults without links", "", Progress{}, StatusNotStarted},
		{"linked tickets not started", StatusDone, Progress{Done: 0, Total: 3}, StatusNotStarted},
		{"linked tickets in progress", StatusNotStarted, Progress{Done: 1, Total: 3}, StatusInProgress},
		{"all linked tickets done", StatusInProgress, Progress{Done: 2, Total: 2}, StatusDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveItemStatus(tc.manual, tc.progress); got != tc.want {
				t.Fatalf("DeriveItemStatus(%q, %+v) = %q, want %q", tc.manual, tc.progress, got, tc.want)
			}
		})
	}
}

func TestValidManualStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		valid  bool
	}{
		{StatusNotStarted, true}, {StatusInProgress, true}, {StatusDone, true}, {"blocked", false}, {"", false},
	} {
		if got := ValidManualStatus(tc.status); got != tc.valid {
			t.Errorf("ValidManualStatus(%q) = %t, want %t", tc.status, got, tc.valid)
		}
	}
}

func TestAggregateProgress(t *testing.T) {
	got := AggregateProgress([]LinkedTicket{
		{ID: "a", Done: true},
		{ID: "b", Done: false},
		{ID: "a", Done: true}, // a ticket linked to multiple items counts once overall
	})
	if got != (Progress{Done: 1, Total: 2}) {
		t.Fatalf("AggregateProgress = %+v, want {Done:1 Total:2}", got)
	}
}

func TestReplaceItemsRejectsInvalidManualStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/api/roadmaps/r/items", strings.NewReader(`[{"title":"Milestone","manual_status":"blocked"}]`))
	req.SetPathValue("id", "r")
	rec := httptest.NewRecorder()
	NewSystem(nil).ReplaceItems(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "manual_status") {
		t.Fatalf("handler response = %d %q, want bad request for invalid status", rec.Code, rec.Body.String())
	}
}

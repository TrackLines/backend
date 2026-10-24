package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestUnplanTicketTool(t *testing.T) {
	var unplan tool
	for _, candidate := range tools {
		if candidate.name == "unplan_ticket" {
			unplan = candidate
			break
		}
	}
	if unplan.name == "" {
		t.Fatal("unplan_ticket is not registered")
	}
	if !strings.Contains(unplan.description, "instead of unplan_sprint") {
		t.Fatalf("description should distinguish unplanning a ticket from removing a sprint: %s", unplan.description)
	}
	if len(unplan.props) != 1 || unplan.props["ticket_id"] == nil || len(unplan.required) != 1 || unplan.required[0] != "ticket_id" {
		t.Fatalf("unplan_ticket should accept only ticket_id: properties=%v required=%v", unplan.props, unplan.required)
	}

	var ticketPlannedSprint, ticketEstimate *string
	initialSprint := "planned-sprint-id"
	ticketPlannedSprint = &initialSprint
	initialEstimate := "5"
	ticketEstimate = &initialEstimate
	called := false
	call := func(_ context.Context, method, path string, body any) (int, []byte) {
		called = true
		if method != http.MethodPut || path != "/api/tickets/00000000-0000-0000-0000-000000000001/planned-sprint" {
			t.Fatalf("unexpected request: %s %s", method, path)
		}
		payload, ok := body.(map[string]any)
		if !ok {
			t.Fatalf("expected JSON body map, got %T", body)
		}
		plannedSprintID, exists := payload["planned_sprint_id"]
		if !exists || plannedSprintID != nil {
			t.Fatalf("unplan_ticket must send planned_sprint_id: null; got %#v", payload)
		}
		// Model the endpoint's existing behavior: the ticket remains in the backlog without an estimate.
		ticketPlannedSprint = nil
		ticketEstimate = nil
		return http.StatusNoContent, nil
	}
	handler := unplan.handler(call)
	result, err := handler(context.Background(), &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{
		Arguments: json.RawMessage(`{"ticket_id":"00000000-0000-0000-0000-000000000001"}`),
	}})
	if err != nil || result.IsError {
		t.Fatalf("unplan_ticket failed: result=%+v err=%v", result, err)
	}
	if !called || ticketPlannedSprint != nil || ticketEstimate != nil {
		t.Fatalf("ticket was not unplanned: called=%v planned_sprint=%v estimate=%v", called, ticketPlannedSprint, ticketEstimate)
	}
}

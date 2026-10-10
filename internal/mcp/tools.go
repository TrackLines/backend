package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func id(what string) map[string]any  { return str(what + " UUID") }

var (
	ticketType = map[string]any{"type": "string", "enum": []string{"bug", "feature", "task"}}
	priority   = map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "urgent"}}
	labels     = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Free-form labels; [] clears"}
	estimate   = str("Estimate on the board's scale (see the board's estimate_scale); \"\" clears")
	position   = map[string]any{"type": "integer", "minimum": 0, "description": "0 = top of the column"}
)

// tools is everything a bot can do over MCP. Org administration, API keys, billing and editing
// columns or roadmaps stay REST/web-only for now.
var tools = []tool{
	// reading
	{name: "whoami", title: "Who am I", description: "The caller's user id, active organization, whether they're an org admin, and the teams they lead.",
		method: "GET", path: "/api/organizations/me", readOnly: true},
	{name: "list_projects", title: "List projects", description: "Projects in the caller's organization, with board counts, open/done tickets and urgent counts.",
		method: "GET", path: "/api/projects", readOnly: true},
	{name: "get_project", title: "Get project", description: "A project with its boards (and per-board progress) and roadmaps.",
		method: "GET", path: "/api/projects/{project_id}", props: map[string]any{"project_id": id("Project")}, required: []string{"project_id"}, readOnly: true},
	{name: "get_board", title: "Get board", description: "A board with its columns (in order; the last is Done) and their tickets, plus its open sprint, style and estimate scale.",
		method: "GET", path: "/api/boards/{board_id}", props: map[string]any{"board_id": id("Board")}, required: []string{"board_id"}, readOnly: true},
	{name: "list_backlog", title: "List backlog", description: "A page of a project's backlog (tickets not on a board), blocked tickets last, with per-type counts and label counts.",
		method: "GET", path: "/api/projects/{project_id}/backlog/page", query: []string{"type", "label", "page", "per_page"},
		props: map[string]any{"project_id": id("Project"), "type": ticketType,
			"label":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only tickets with any of these labels"},
			"page":     map[string]any{"type": "integer", "minimum": 1},
			"per_page": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}},
		required: []string{"project_id"}, readOnly: true},
	{name: "list_open_tickets", title: "List open tickets", description: "The way to find what to work on next: every ticket in a project that isn't done, across all boards and the backlog, " +
		"without descriptions, already in pick order (in an open sprint first, bugs before other ticket types within that sprint, then priority urgent→low; " +
		"out-of-sprint tickets follow, ordered by priority urgent→low, then on a board before the backlog, then oldest). " +
		"in_sprint marks tickets in their board's open sprint. " +
		"Use assignee=unassigned and blocked=exclude for claimable work, or assignee=me for what you hold; then get_ticket for details.",
		method: "GET", path: "/api/projects/{project_id}/open-tickets", query: []string{"assignee", "blocked"},
		props: map[string]any{"project_id": id("Project"),
			"assignee": map[string]any{"type": "string", "enum": []string{"unassigned", "me"}, "description": "unassigned, or me (the caller's agent name / user id); omit for everyone"},
			"blocked":  map[string]any{"type": "string", "enum": []string{"exclude"}, "description": "exclude leaves out tickets waiting on unfinished work"}},
		required: []string{"project_id"}, readOnly: true},
	{name: "get_ticket", title: "Get ticket", description: "A ticket with where it lives (project, board, column, sprint), its blockers, sub-tickets and parent.",
		method: "GET", path: "/api/tickets/{ticket_id}", props: map[string]any{"ticket_id": id("Ticket")}, required: []string{"ticket_id"}, readOnly: true},
	{name: "list_comments", title: "List comments", description: "A ticket's comments, oldest first.",
		method: "GET", path: "/api/tickets/{ticket_id}/comments", props: map[string]any{"ticket_id": id("Ticket")}, required: []string{"ticket_id"}, readOnly: true},
	{name: "list_assignees", title: "List assignees", description: "Who tickets can be assigned to.",
		method: "GET", path: "/api/assignees", readOnly: true},
	{name: "list_project_labels", title: "List project labels", description: "Labels used in a project, with counts.",
		method: "GET", path: "/api/projects/{project_id}/labels", props: map[string]any{"project_id": id("Project")}, required: []string{"project_id"}, readOnly: true},
	{name: "list_sprints", title: "List sprints", description: "A board's sprints, newest first.",
		method: "GET", path: "/api/boards/{board_id}/sprints", props: map[string]any{"board_id": id("Board")}, required: []string{"board_id"}, readOnly: true},
	{name: "get_velocity", title: "Get velocity", description: "What each closed sprint finished (points or tickets) and the open sprint's burn data.",
		method: "GET", path: "/api/boards/{board_id}/velocity", props: map[string]any{"board_id": id("Board")}, required: []string{"board_id"}, readOnly: true},
	{name: "list_roadmaps", title: "List roadmaps", description: "Roadmaps in the caller's organization.",
		method: "GET", path: "/api/roadmaps", readOnly: true},
	{name: "get_roadmap", title: "Get roadmap", description: "A roadmap with its items and their linked tickets.",
		method: "GET", path: "/api/roadmaps/{roadmap_id}", props: map[string]any{"roadmap_id": id("Roadmap")}, required: []string{"roadmap_id"}, readOnly: true},

	// tickets
	{name: "create_ticket", title: "Create ticket", description: "Create a ticket in a board column (joins the board's open sprint). " +
		"On the sprint's last day its scope is locked: the ticket goes to the bottom of the backlog instead (redirected_to_backlog: true, column_id null). " +
		"On a board with an estimate scale, estimate is required.",
		method: "POST", path: "/api/columns/{column_id}/tickets", body: []string{"title", "description", "type", "priority", "labels", "estimate"},
		props:    map[string]any{"column_id": id("Column"), "title": str("Title"), "description": str("Details (markdown)"), "type": ticketType, "priority": priority, "labels": labels, "estimate": estimate},
		required: []string{"column_id", "title"}},
	{name: "create_backlog_ticket", title: "Create backlog ticket", description: "Add a ticket to the bottom of a project's backlog (no estimate until it reaches a board).",
		method: "POST", path: "/api/projects/{project_id}/backlog", body: []string{"title", "description", "type", "priority", "labels"},
		props:    map[string]any{"project_id": id("Project"), "title": str("Title"), "description": str("Details (markdown)"), "type": ticketType, "priority": priority, "labels": labels},
		required: []string{"project_id", "title"}},
	{name: "update_ticket", title: "Update ticket", description: "Change a ticket's title, description, type, priority, labels or estimate. Omitted fields stay as they are.",
		method: "PATCH", path: "/api/tickets/{ticket_id}", body: []string{"title", "description", "type", "priority", "labels", "estimate"},
		props:    map[string]any{"ticket_id": id("Ticket"), "title": str("New title"), "description": str("New description (markdown)"), "type": ticketType, "priority": priority, "labels": labels, "estimate": estimate},
		required: []string{"ticket_id"}, prepare: keepTitleAndDescription},
	{name: "claim_ticket", title: "Claim ticket", description: "Assign an unassigned, unblocked ticket to the caller, atomically. Fails if it's already assigned or blocked.",
		method: "POST", path: "/api/tickets/{ticket_id}/claim", props: map[string]any{"ticket_id": id("Ticket")}, required: []string{"ticket_id"}},
	{name: "release_ticket", title: "Release ticket", description: "Unassign a ticket the caller holds.",
		method: "POST", path: "/api/tickets/{ticket_id}/release", props: map[string]any{"ticket_id": id("Ticket")}, required: []string{"ticket_id"}},
	{name: "assign_ticket", title: "Assign ticket", description: "Assign a ticket to someone from list_assignees, or null to unassign.",
		method: "PUT", path: "/api/tickets/{ticket_id}/assignee", body: []string{"assignee"},
		props: map[string]any{"ticket_id": id("Ticket"), "assignee": map[string]any{"type": []string{"string", "null"}}}, required: []string{"ticket_id"}},
	{name: "move_ticket", title: "Move ticket", description: "Move a ticket to a column (any board in the same project, which also takes it out of the backlog). Moving to the board's last column marks it done. " +
		"On a sprint's last day, moving a ticket into that sprint is refused (scope locked); moves within it or out are fine. " +
		"A board with an estimate scale only takes estimated tickets: moving one in from the backlog or another board needs estimate (on the target board's scale, see get_board) " +
		"unless it already has one on the same scale.",
		method: "POST", path: "/api/tickets/{ticket_id}/move", body: []string{"column_id", "position", "estimate"},
		props: map[string]any{"ticket_id": id("Ticket"), "column_id": id("Destination column"), "position": position, "estimate": str("Estimate on the target board's scale; required entering a board with an estimate scale")}, required: []string{"ticket_id", "column_id"}},
	{name: "send_to_backlog", title: "Send to backlog", description: "Take a ticket off its board and put it at the bottom of the project backlog (clears its estimate).",
		method: "POST", path: "/api/tickets/{ticket_id}/backlog", props: map[string]any{"ticket_id": id("Ticket")}, required: []string{"ticket_id"}},
	{name: "resolve_ticket", title: "Resolve ticket", description: "Close a backlog ticket in place, without putting it on a board (a board ticket is finished by moving it to the board's last column). " +
		"It counts as done, leaves the backlog and open-ticket lists, and stays readable by id. If it was the last unfinished sub-ticket, its parent is completed too.",
		method: "POST", path: "/api/tickets/{ticket_id}/resolve", props: map[string]any{"ticket_id": id("Ticket")}, required: []string{"ticket_id"}},
	{name: "set_labels", title: "Set labels", description: "Replace a ticket's labels.",
		method: "PUT", path: "/api/tickets/{ticket_id}/labels", body: []string{"labels"},
		props: map[string]any{"ticket_id": id("Ticket"), "labels": labels}, required: []string{"ticket_id", "labels"}},
	{name: "add_comment", title: "Add comment", description: "Comment on a ticket (markdown), e.g. to report progress or what was done.",
		method: "POST", path: "/api/tickets/{ticket_id}/comments", body: []string{"body"},
		props: map[string]any{"ticket_id": id("Ticket"), "body": str("Comment (markdown)")}, required: []string{"ticket_id", "body"}},
	{name: "set_blocked_by", title: "Set blockers", description: "Replace the tickets this one waits on (same project, no cycles); [] clears. Blocked tickets can't be claimed.",
		method: "PUT", path: "/api/tickets/{ticket_id}/blocked-by", body: []string{"ticket_ids"},
		props: map[string]any{"ticket_id": id("Ticket"), "ticket_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, required: []string{"ticket_id", "ticket_ids"}},
	{name: "set_parent", title: "Set parent", description: "Make a ticket a sub-ticket of parent_id, or null to detach.",
		method: "PUT", path: "/api/tickets/{ticket_id}/parent", body: []string{"parent_id"},
		props: map[string]any{"ticket_id": id("Ticket"), "parent_id": map[string]any{"type": []string{"string", "null"}}}, required: []string{"ticket_id"}},
	{name: "delete_ticket", title: "Delete ticket", description: "Permanently delete a ticket.",
		method: "DELETE", path: "/api/tickets/{ticket_id}", props: map[string]any{"ticket_id": id("Ticket")}, required: []string{"ticket_id"}, destructive: true},

	// sprints
	{name: "start_sprint", title: "Start sprint", description: "Open a sprint on a (non-kanban) board; tickets already on it join.",
		method: "POST", path: "/api/boards/{board_id}/sprints", body: []string{"length_days"},
		props: map[string]any{"board_id": id("Board"), "length_days": map[string]any{"type": "integer", "minimum": 1, "maximum": 365}}, required: []string{"board_id", "length_days"}},
	{name: "update_sprint_length", title: "Change sprint length", description: "Change an open or scheduled sprint's length in days; its start date stays fixed.",
		method: "PATCH", path: "/api/sprints/{sprint_id}", body: []string{"length_days"},
		props: map[string]any{"sprint_id": id("Sprint"), "length_days": map[string]any{"type": "integer", "minimum": 1, "maximum": 365}}, required: []string{"sprint_id", "length_days"}},
	{name: "close_sprint", title: "Close sprint", description: "Close an open sprint and carry unfinished tickets forward. Optionally set next_length_days and next_starts_at (RFC3339); by default the next sprint starts now with the same length.",
		method: "POST", path: "/api/sprints/{sprint_id}/close", body: []string{"next_length_days", "next_starts_at"},
		props: map[string]any{"sprint_id": id("Sprint"), "next_length_days": map[string]any{"type": "integer", "minimum": 1, "maximum": 365}, "next_starts_at": map[string]any{"type": "string", "format": "date-time"}}, required: []string{"sprint_id"}},

	// refinement: plan backlog tickets into a board's next sprints and size them
	{name: "list_planned_sprints", title: "List planned sprints", description: "A board's planned sprints (up to 2, after the current one), with their tickets and capacity: " +
		"velocity (average of the last 3 closed sprints), cap (velocity + 10%), used, unestimated, and whether it's over and needs approval.",
		method: "GET", path: "/api/boards/{board_id}/planned-sprints", props: map[string]any{"board_id": id("Board")}, required: []string{"board_id"}, readOnly: true},
	{name: "plan_sprint", title: "Plan a sprint", description: "Add a planned sprint after the board's current (and any planned) sprint; at most 2. Org admins and the board team's leaders.",
		method: "POST", path: "/api/boards/{board_id}/planned-sprints", body: []string{"length_days"},
		props: map[string]any{"board_id": id("Board"), "length_days": map[string]any{"type": "integer", "minimum": 1, "maximum": 365}}, required: []string{"board_id", "length_days"}},
	{name: "plan_ticket", title: "Plan ticket into a sprint", description: "Put a backlog ticket into a planned sprint (it stays in the backlog until that sprint starts), sized with estimate " +
		"on that board's scale; planned_sprint_id null takes it out. Going over capacity is allowed but the sprint then needs approval before it can start.",
		method: "PUT", path: "/api/tickets/{ticket_id}/planned-sprint", body: []string{"planned_sprint_id", "estimate"},
		props: map[string]any{"ticket_id": id("Ticket"), "planned_sprint_id": map[string]any{"type": []string{"string", "null"}, "description": "Planned sprint UUID, or null to unplan"},
			"estimate": str("Estimate on the planned sprint's board scale")}, required: []string{"ticket_id", "planned_sprint_id"}},
	{name: "approve_planned_sprint", title: "Approve planned sprint", description: "Accept a planned sprint's current total over capacity, so it can start. Org admins and the board team's leaders " +
		"(signed in; agent keys can't approve). Adding more afterwards needs approval again.",
		method: "POST", path: "/api/planned-sprints/{planned_sprint_id}/approve", props: map[string]any{"planned_sprint_id": id("Planned sprint")}, required: []string{"planned_sprint_id"}},
	{name: "unplan_sprint", title: "Remove planned sprint", description: "Remove a planned sprint; its tickets stay in the backlog, unplanned and unestimated. Org admins and the board team's leaders.",
		method: "DELETE", path: "/api/planned-sprints/{planned_sprint_id}", props: map[string]any{"planned_sprint_id": id("Planned sprint")}, required: []string{"planned_sprint_id"}},
}

// keepTitleAndDescription: PATCH /tickets/{id} replaces both, so fill whichever the bot left out.
func keepTitleAndDescription(ctx context.Context, call caller, args map[string]any) error {
	_, hasTitle := args["title"]
	_, hasDesc := args["description"]
	if hasTitle && hasDesc {
		return nil
	}
	status, out := call(ctx, http.MethodGet, "/api/tickets/"+fmt.Sprint(args["ticket_id"]), nil)
	if status != http.StatusOK {
		return fmt.Errorf("%d %s: couldn't read the ticket", status, http.StatusText(status))
	}
	var current struct{ Title, Description string }
	if err := json.Unmarshal(out, &current); err != nil {
		return err
	}
	if !hasTitle {
		args["title"] = current.Title
	}
	if !hasDesc {
		args["description"] = current.Description
	}
	return nil
}

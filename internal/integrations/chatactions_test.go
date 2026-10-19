package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/notifications"
)

// fakeAPI is TrackLin.es's API as the Chat app's buttons see it: a ticket on a board, recording calls.
type fakeAPI struct {
	calls  []string // "user org METHOD path body"
	column string   // the ticket's column
	board  *string
	refuse int // answer writes with this status and a reason
}

func (f *fakeAPI) act(_ context.Context, user, org, method, path string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	if body == nil {
		b = nil
	}
	f.calls = append(f.calls, strings.TrimSpace(user+" "+org+" "+method+" "+path+" "+string(b)))
	switch {
	case method == http.MethodGet && path == "/api/tickets/t1":
		out, _ := json.Marshal(map[string]any{"key": "TL-12", "board_id": f.board, "column_id": f.column})
		return 200, out
	case method == http.MethodGet && path == "/api/tickets/nope":
		return 404, []byte("not found\n")
	case method == http.MethodGet && path == "/api/boards/b1":
		return 200, []byte(`{"columns":[{"id":"todo","name":"To do"},{"id":"doing","name":"In progress"},{"id":"done","name":"Done"}]}`)
	case f.refuse != 0:
		return f.refuse, []byte("only people review agent work\n")
	}
	return 204, nil
}

// Buttons on personal Chat messages (TL-455): each kind gets its own; a click acts as the linked person
// through the API (their permissions), only while they're in the org; dialogs ask for words first.
func TestChatButtons(t *testing.T) {
	ctx := context.Background()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+dbURL[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM chat_dms WHERE google_user IN ('users/btn', 'users/left');
		INSERT INTO chat_dms (user_clerk_id, google_user, space) VALUES ('user_btn', 'users/btn', 'spaces/B'), ('user_left', 'users/left', 'spaces/L')`); err != nil {
		t.Fatal(err)
	}
	board := "b1"
	api := &fakeAPI{column: "todo", board: &board}
	app := &ChatApp{DB: db, Endpoint: "https://api.tracklin.es/api/webhooks/google-chat", Act: api.act,
		Member: func(_ context.Context, org, user string) (bool, error) {
			return org == "org_x" && user != "user_left", nil
		}}
	text := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	click := func(google, do, dialog, typed string, addon bool) string {
		api.calls = nil
		return text(app.clicked(ctx, google, chatClick{do: do, org: "org_x", ticket: "t1", dialog: dialog, text: typed}, addon))
	}

	// which buttons each message gets, then Open; nothing to act on without a ticket
	labels := func(kind, ticket string) (out []string) {
		for _, b := range app.chatButtons("org_x", notifications.Event{Kind: kind, TicketID: ticket}, "https://tracklin.es/tickets/t1") {
			out = append(out, b.(map[string]any)["text"].(string))
		}
		return out
	}
	for kind, want := range map[string]string{
		"assigned": "Start,Add comment,Make urgent,Open in TrackLin.es", "mention": "Reply,Make urgent,Open in TrackLin.es",
		"review": "Approve,Send back,Open in TrackLin.es", "sent_back": "Add comment,Make urgent,Open in TrackLin.es", "finished": "Open in TrackLin.es",
	} {
		if got := strings.Join(labels(kind, "t1"), ","); got != want {
			t.Errorf("%s: %s, want %s", kind, got, want)
		}
	}
	if got := labels("assigned", ""); len(got) != 1 {
		t.Errorf("no ticket: %v", got)
	}
	button := text(app.chatButtons("org_x", notifications.Event{Kind: "assigned", TicketID: "t1"}, "")[1])
	for _, s := range []string{`"function":"https://api.tracklin.es/api/webhooks/google-chat"`, `"interaction":"OPEN_DIALOG"`, `{"key":"org","value":"org_x"}`, `{"key":"ticket","value":"t1"}`} {
		if !strings.Contains(button, s) {
			t.Errorf("comment button lacks %s: %s", s, button)
		}
	}

	// who: only a linked person, still in the org; nothing runs otherwise
	if got := click("users/stranger", "urgent", "", "", false); !strings.Contains(got, "Link your TrackLin.es account first") || len(api.calls) != 0 {
		t.Fatalf("unlinked: %s %v", got, api.calls)
	}
	if got := click("users/left", "urgent", "", "", false); !strings.Contains(got, "not in that organization") || len(api.calls) != 0 {
		t.Fatalf("left the org: %s %v", got, api.calls)
	}
	if got := click("users/btn", "delete-everything", "", "", false); !strings.Contains(got, "doesn’t do anything") || len(api.calls) != 0 {
		t.Fatalf("unknown action: %s %v", got, api.calls)
	}

	// Make urgent and Approve: straight through, as the linked person in that org
	if got := click("users/btn", "urgent", "", "", false); !strings.Contains(got, "TL-12 is urgent now.") ||
		api.calls[1] != `user_btn org_x PATCH /api/tickets/t1 {"priority":"urgent"}` {
		t.Fatalf("urgent: %s %v", got, api.calls)
	}
	if got := click("users/btn", "approve", "", "", true); !strings.Contains(got, `"createMessageAction"`) || !strings.Contains(got, "Approved TL-12.") ||
		api.calls[1] != `user_btn org_x POST /api/tickets/t1/review {"approve":true}` {
		t.Fatalf("approve (add-on): %s %v", got, api.calls)
	}

	// Start: from the board's first column into its second; not twice, not from the backlog
	if got := click("users/btn", "start", "", "", false); !strings.Contains(got, "Moved TL-12 to In progress.") ||
		api.calls[2] != `user_btn org_x POST /api/tickets/t1/move {"column_id":"doing"}` {
		t.Fatalf("start: %s %v", got, api.calls)
	}
	api.column = "doing"
	if got := click("users/btn", "start", "", "", false); !strings.Contains(got, "already started") || len(api.calls) != 2 {
		t.Fatalf("started twice: %s %v", got, api.calls)
	}
	api.board = nil
	if got := click("users/btn", "start", "", "", false); !strings.Contains(got, "in the backlog") || len(api.calls) != 1 {
		t.Fatalf("backlog: %s %v", got, api.calls)
	}

	// Add comment: a dialog first (either format), then the words, posted as them; empty adds nothing
	if got := click("users/btn", "comment", "REQUEST_DIALOG", "", false); !strings.Contains(got, `"type":"DIALOG"`) || !strings.Contains(got, `"textInput"`) || len(api.calls) != 1 {
		t.Fatalf("comment dialog: %s %v", got, api.calls)
	}
	if got := click("users/btn", "reply", "REQUEST_DIALOG", "", true); !strings.Contains(got, `"pushCard"`) || !strings.Contains(got, "Comment on TL-12") {
		t.Fatalf("reply dialog (add-on): %s", got)
	}
	if got := click("users/btn", "comment", "SUBMIT_DIALOG", "On it", false); !strings.Contains(got, `"userFacingMessage":"Comment added to TL-12."`) ||
		api.calls[1] != `user_btn org_x POST /api/tickets/t1/comments {"body":"On it"}` {
		t.Fatalf("comment: %s %v", got, api.calls)
	}
	if got := click("users/btn", "comment", "SUBMIT_DIALOG", "", true); !strings.Contains(got, "CLOSE_DIALOG") || !strings.Contains(got, "comment was empty") || len(api.calls) != 1 {
		t.Fatalf("empty comment: %s %v", got, api.calls)
	}
	if got := click("users/btn", "comment", "CANCEL_DIALOG", "", false); !strings.Contains(got, `"statusCode":"OK"`) || len(api.calls) != 0 {
		t.Fatalf("cancel: %s %v", got, api.calls)
	}
	// Send back: the reason is optional
	if got := click("users/btn", "sendback", "SUBMIT_DIALOG", "", false); !strings.Contains(got, "Sent TL-12 back.") ||
		api.calls[1] != `user_btn org_x POST /api/tickets/t1/review {"approve":false,"comment":""}` {
		t.Fatalf("send back: %s %v", got, api.calls)
	}

	// the API's refusal is passed on in words; a ticket they can't see is said so
	api.refuse = http.StatusForbidden
	if got := click("users/btn", "approve", "", "", false); !strings.Contains(got, "Couldn’t do that on TL-12: only people review agent work") {
		t.Fatalf("refused: %s", got)
	}
	api.refuse = 0
	api.calls = nil
	if got := text(app.clicked(ctx, "users/btn", chatClick{do: "urgent", org: "org_x", ticket: "nope"}, false)); !strings.Contains(got, "couldn’t find that ticket: not found") {
		t.Fatalf("missing ticket: %s", got)
	}

	// both of Chat's formats read as a click, with the button's parameters and what was typed
	var classic, addon chatRequest
	_ = json.Unmarshal([]byte(`{"type":"CARD_CLICKED","user":{"name":"users/btn"},"isDialogEvent":true,"dialogEventType":"SUBMIT_DIALOG",
		"common":{"parameters":{"do":"comment","org":"org_x","ticket":"t1"},"formInputs":{"text":{"stringInputs":{"value":["  Looks good  "]}}}}}`), &classic)
	_ = json.Unmarshal([]byte(`{"commonEventObject":{"parameters":{"do":"urgent","org":"org_x","ticket":"t1"}},
		"chat":{"user":{"name":"users/btn"},"buttonClickedPayload":{"space":{"name":"spaces/B"}}}}`), &addon)
	if kind, user, _, isAddon := classic.event(); kind != "click" || user.Name != "users/btn" || isAddon {
		t.Fatalf("classic: %s %s %v", kind, user.Name, isAddon)
	}
	if k := classic.click(); k != (chatClick{do: "comment", org: "org_x", ticket: "t1", dialog: "SUBMIT_DIALOG", text: "Looks good"}) {
		t.Fatalf("classic click: %+v", k)
	}
	if kind, user, space, isAddon := addon.event(); kind != "click" || user.Name != "users/btn" || space.Name != "spaces/B" || !isAddon {
		t.Fatalf("add-on: %s %s %s %v", kind, user.Name, space.Name, isAddon)
	}
	if k := addon.click(); k != (chatClick{do: "urgent", org: "org_x", ticket: "t1"}) {
		t.Fatalf("add-on click: %+v", k)
	}
}

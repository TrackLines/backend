package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5"
	"github.com/tracklines/backend/internal/notifications"
)

// Buttons on the Chat app's personal messages (TL-455), like Bitbucket's "Add comment": Start, Add
// comment / Reply, Make urgent, Approve and Send back. A click comes back to Events signed by Google,
// and runs as the TrackLin.es account linked to the Google account that clicked, through the same API
// routes and permissions as the web app. Team spaces get webhook posts, which can't have buttons.

// chatActions are the buttons each kind of personal message gets, in order.
var chatActions = map[string][]string{
	"assigned":  {"start", "comment", "urgent"},
	"mention":   {"reply", "urgent"},
	"review":    {"approve", "sendback"},
	"sent_back": {"comment", "urgent"},
}

var chatActionLabels = map[string]string{
	"start": "Start", "comment": "Add comment", "reply": "Reply", "urgent": "Make urgent", "approve": "Approve", "sendback": "Send back",
}

// dialogs are the actions that ask for words first: a comment, or why it's going back.
var chatDialogs = map[string]bool{"comment": true, "reply": true, "sendback": true}

// chatButtons are e's action buttons for a personal message, then "Open in TrackLin.es".
func (c *ChatApp) chatButtons(org string, e notifications.Event, link string) []any {
	var out []any
	if e.TicketID != "" {
		for _, do := range chatActions[e.Kind] {
			action := map[string]any{"function": c.Endpoint, "parameters": []any{
				map[string]any{"key": "do", "value": do}, map[string]any{"key": "org", "value": org}, map[string]any{"key": "ticket", "value": e.TicketID},
			}}
			if chatDialogs[do] {
				action["interaction"] = "OPEN_DIALOG"
			}
			out = append(out, map[string]any{"text": chatActionLabels[do], "onClick": map[string]any{"action": action}})
		}
	}
	if link != "" {
		out = append(out, map[string]any{"text": "Open in TrackLin.es", "onClick": map[string]any{"openLink": map[string]any{"url": link}}})
	}
	return out
}

// chatClick is a button press: what to do, on which ticket in which org, and for dialogs whether it's
// asking for the dialog or sending it, with what was typed.
type chatClick struct {
	do, org, ticket string
	dialog          string // "" | REQUEST_DIALOG | SUBMIT_DIALOG | CANCEL_DIALOG
	text            string
}

type chatCommon struct {
	Parameters map[string]string `json:"parameters"`
	FormInputs map[string]struct {
		StringInputs struct {
			Value []string `json:"value"`
		} `json:"stringInputs"`
	} `json:"formInputs"`
}

func (q chatRequest) click() chatClick {
	common, dialog := q.Common, q.DialogEventType
	if q.Chat != nil && q.Chat.Clicked != nil {
		common, dialog = q.CommonEventObject, q.Chat.Clicked.DialogEventType
	}
	k := chatClick{do: common.Parameters["do"], org: common.Parameters["org"], ticket: common.Parameters["ticket"], dialog: dialog}
	if in := common.FormInputs["text"].StringInputs.Value; len(in) > 0 {
		k.text = strings.TrimSpace(in[0])
	}
	return k
}

// clicked runs a button press for the Google user, and answers in the request's format.
func (c *ChatApp) clicked(ctx context.Context, google string, k chatClick, addon bool) any {
	say := func(text string) any {
		if k.dialog == "SUBMIT_DIALOG" {
			return chatCloseDialog(addon, text)
		}
		return chatReply("message", addon, text, "", "")
	}
	if k.dialog == "CANCEL_DIALOG" {
		return chatCloseDialog(addon, "")
	}
	if _, known := chatActionLabels[k.do]; !known || k.org == "" || k.ticket == "" {
		return say("That button doesn’t do anything any more.")
	}
	var user string
	err := c.DB.QueryRow(ctx, `SELECT user_clerk_id FROM chat_dms WHERE google_user = $1`, google).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return say("Link your TrackLin.es account first: send me a message and I’ll send you a link.")
	}
	if err != nil {
		logs.Errorf("google chat app: click: %v", err)
		return say("Something went wrong. Please try again.")
	}
	// someone who has left the organization can't act in it, whatever the card says
	if c.Member != nil {
		if in, err := c.Member(ctx, k.org, user); err != nil || !in {
			return say("You’re not in that organization any more.")
		}
	}
	if c.Act == nil {
		return say("Buttons aren’t available on this server.")
	}
	var t struct {
		Key      string  `json:"key"`
		BoardID  *string `json:"board_id"`
		ColumnID string  `json:"column_id"`
	}
	status, body := c.Act(ctx, user, k.org, http.MethodGet, "/api/tickets/"+url.PathEscape(k.ticket), nil)
	if status != http.StatusOK || json.Unmarshal(body, &t) != nil {
		return say("I couldn’t find that ticket: " + chatProblem(status, body))
	}
	if chatDialogs[k.do] && k.dialog != "SUBMIT_DIALOG" {
		return chatDialog(c.Endpoint, k, t.Key, addon)
	}
	if chatDialogs[k.do] && k.text == "" && k.do != "sendback" {
		return say("Nothing to add: the comment was empty.")
	}
	var method, path, done string
	var in any
	switch k.do {
	case "comment", "reply":
		method, path, in, done = http.MethodPost, "/comments", map[string]any{"body": k.text}, "Comment added to "+t.Key+"."
	case "urgent":
		method, path, in, done = http.MethodPatch, "", map[string]any{"priority": "urgent"}, t.Key+" is urgent now."
	case "approve":
		method, path, in, done = http.MethodPost, "/review", map[string]any{"approve": true}, "Approved "+t.Key+"."
	case "sendback":
		method, path, in, done = http.MethodPost, "/review", map[string]any{"approve": false, "comment": k.text}, "Sent "+t.Key+" back."
	case "start":
		column, name, why := c.startColumn(ctx, user, k.org, t.BoardID, t.ColumnID)
		if why != "" {
			return say(t.Key + " " + why)
		}
		method, path, in, done = http.MethodPost, "/move", map[string]any{"column_id": column}, "Moved "+t.Key+" to "+name+"."
	}
	if status, body := c.Act(ctx, user, k.org, method, "/api/tickets/"+url.PathEscape(k.ticket)+path, in); status >= 300 {
		return say("Couldn’t do that on " + t.Key + ": " + chatProblem(status, body))
	}
	return say(done)
}

// startColumn is the board's second column (where work starts), or why the ticket can't start.
func (c *ChatApp) startColumn(ctx context.Context, user, org string, board *string, column string) (id, name, why string) {
	if board == nil {
		return "", "", "is in the backlog: put it on a board first."
	}
	var b struct {
		Columns []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"columns"`
	}
	status, body := c.Act(ctx, user, org, http.MethodGet, "/api/boards/"+url.PathEscape(*board), nil)
	if status != http.StatusOK || json.Unmarshal(body, &b) != nil || len(b.Columns) < 2 {
		return "", "", "couldn’t be started: " + chatProblem(status, body)
	}
	if b.Columns[0].ID != column {
		return "", "", "has already started."
	}
	return b.Columns[1].ID, b.Columns[1].Name, ""
}

// chatProblem is the API's reason, in a line.
func chatProblem(status int, body []byte) string {
	if msg := strings.TrimSpace(string(body)); msg != "" && status < 500 && len(msg) < 300 && !strings.HasPrefix(msg, "{") {
		return msg
	}
	return "something went wrong. Please try again."
}

// chatDialog asks for the words: a comment, or why it's being sent back.
func chatDialog(endpoint string, k chatClick, key string, addon bool) any {
	label, button := "Comment on "+key, "Add comment"
	if k.do == "sendback" {
		label, button = "Why is "+key+" going back? (optional)", "Send back"
	}
	card := map[string]any{"sections": []any{map[string]any{"widgets": []any{
		map[string]any{"textInput": map[string]any{"name": "text", "label": label, "type": "MULTIPLE_LINE"}},
		map[string]any{"buttonList": map[string]any{"buttons": []any{map[string]any{"text": button, "onClick": map[string]any{"action": map[string]any{
			"function": endpoint, "parameters": []any{
				map[string]any{"key": "do", "value": k.do}, map[string]any{"key": "org", "value": k.org}, map[string]any{"key": "ticket", "value": k.ticket},
			}}}}}}},
	}}}}
	if addon {
		return map[string]any{"action": map[string]any{"navigations": []any{map[string]any{"pushCard": card}}}}
	}
	return map[string]any{"actionResponse": map[string]any{"type": "DIALOG", "dialogAction": map[string]any{"dialog": map[string]any{"body": card}}}}
}

// chatCloseDialog closes the dialog, saying how it went.
func chatCloseDialog(addon bool, text string) any {
	if addon {
		out := map[string]any{"action": map[string]any{"navigations": []any{map[string]any{"endNavigation": map[string]any{"action": "CLOSE_DIALOG"}}}}}
		if text != "" {
			out["action"].(map[string]any)["notification"] = map[string]any{"text": text}
		}
		return out
	}
	return map[string]any{"actionResponse": map[string]any{"type": "DIALOG", "dialogAction": map[string]any{"actionStatus": map[string]any{"statusCode": "OK", "userFacingMessage": text}}}}
}

// Package mcp serves TrackLines over the Model Context Protocol (Streamable HTTP, stateless), so
// bots (Claude, Codex, …) work with the same data as the web app. Every tool is a REST route
// called in-process through the full handler chain, with the caller's own headers: same auth, org
// scoping, validation and errors as REST, and nothing to keep in sync.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tracklines/backend/internal/attachments"
	"github.com/tracklines/backend/internal/tickets"
)

// tool maps one MCP tool onto a REST route. {name} placeholders in path are filled from the
// arguments; query and body list the arguments sent as query parameters or JSON body fields.
type tool struct {
	name, title, description string
	method, path             string
	query, body              []string
	rawBody                  string         // argument sent directly as the complete JSON request body
	props                    map[string]any // JSON Schema properties
	required                 []string
	readOnly, destructive    bool
	prepare                  func(ctx context.Context, call caller, args map[string]any) error // optional: fill args before the call
}

// caller runs an API request as the MCP client.
type caller func(ctx context.Context, method, path string, body any) (int, []byte)

// file is a request body sent as it is, with its own content type, not as JSON (add_attachment).
type file struct {
	data        []byte
	contentType string
}

// Handler is the /mcp endpoint. api is the whole REST handler chain (auth middleware included).
// Instructions tell agents how to work: like a member of the team, the whole way through a ticket. Staff
// publish new versions (TL-674, mcp_prompts, seeded with this text as version 1); this is the fallback for
// when they can't be read.
const Instructions = "TrackLin.es boards, backlog and tickets. Work is organised as projects → boards (one per team) → columns → tickets; " +
	"the first column is To do and the last column is Done (get_board lists a board's columns in order). " +
	"Tickets have keys like TL-123 (project prefix + number); any ticket_id argument takes a key or a UUID.\n\n" +
	"Choosing work: act like a member of the team. While a sprint is open, only pick tickets in it (in_sprint=true in list_open_tickets), " +
	"bugs before features and tasks, highest priority first. Don't start backlog or out-of-sprint tickets unless no sprint ticket is " +
	"claimable or the user asks for that ticket; a backlog ticket goes onto its board first (move_ticket, with an estimate if the board uses them). " +
	"People on holiday show as away in list_assignees: don't assign them work, or ask them to review it, unless you're asked to.\n\n" +
	"Working a ticket: always do the whole chain, in this order, and don't skip steps:\n" +
	"1. Read all of it: get_ticket, list_comments, and when attachment_count > 0, list_attachments and open the files " +
	"(often a design, screenshot or log that explains the ticket better than its text; build what they show, and if they and the " +
	"description disagree, ask or follow the newer one).\n" +
	"2. Assign it to yourself: claim_ticket (it fails if someone else has it or it's blocked: pick another).\n" +
	"3. Set it in progress: move_ticket to the board's in-progress column as you start: claim_ticket's in_progress_column_id, the column " +
	"after To do. When it's null the board has only To do and Done: leave the ticket in To do while you work (never move it to Done to start it).\n" +
	"4. Do the work, and write or update tests that cover it.\n" +
	"5. Run the tests and make sure they pass (the whole suite, not just the new ones). Don't finish a ticket with failing or skipped tests.\n" +
	"6. Comment what you did: add_comment with what changed, where, and the test results.\n" +
	"7. Finish it: move_ticket to the board's last column. On a board with agent review (get_board: agent_review) this puts it " +
	"in the board's review column instead (get_board: review_column_id; the column before Done when that's null; the response says needs_review) and a person approves it; otherwise it's Done. Never leave finished work in To do or In progress.\n" +
	"If you stop before finishing, say why in a comment and release_ticket so someone else can pick it up."

// prompts gives each session the latest published pre-prompt (TL-674); nil: the built-in Instructions.
func Handler(api http.Handler, version string, prompts *PromptStore) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		s := sdk.NewServer(&sdk.Implementation{Name: "tracklines", Title: "TrackLin.es", Version: version}, &sdk.ServerOptions{
			Instructions: prompts.Instructions(r.Context()),
		})
		call := requester(api, r.Header)
		for _, t := range tools {
			s.AddTool(t.spec(), t.handler(call))
		}
		return s
	}, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true,
		MaxRequestBodyBytes: maxRequestBytes}) // room for add_attachment's largest file, as base64
}

// maxRequestBytes fits a 10 MB attachment as base64 (a third larger) with the rest of the message.
const maxRequestBytes = attachments.MaxFileBytes*4/3 + 1<<20

// attachFile decodes add_attachment's content_base64 (a data: URL works too) into the request body.
func attachFile(_ context.Context, _ caller, args map[string]any) error {
	encoded, _ := args["content_base64"].(string)
	if _, after, ok := strings.Cut(encoded, ";base64,"); ok && strings.HasPrefix(encoded, "data:") {
		encoded = after
	}
	encoded = strings.Join(strings.Fields(encoded), "")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(encoded); err != nil {
			return errors.New("content_base64 isn't valid base64")
		}
	}
	if len(data) > attachments.MaxFileBytes {
		return errors.New("files can be up to 10 MB")
	}
	contentType, _ := args["content_type"].(string)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	args["file"] = file{data: data, contentType: contentType}
	delete(args, "content_base64")
	return nil
}

// requester calls the REST API in-process with the MCP request's auth headers.
func requester(api http.Handler, header http.Header) caller {
	return func(ctx context.Context, method, path string, body any) (int, []byte) {
		var buf bytes.Buffer
		contentType := "application/json"
		switch b := body.(type) {
		case nil:
		case file:
			buf.Write(b.data)
			contentType = b.contentType
		default:
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequestWithContext(ctx, method, path, &buf)
		// the caller's address too, so per-address limits count each caller, not every MCP call as one (TL-251)
		for _, h := range []string{"Authorization", "X-Request-Id", "X-Forwarded-For"} {
			if v := header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
}

func (t tool) spec() *sdk.Tool {
	no := false
	required := t.required
	if required == nil {
		required = []string{}
	}
	props := t.props
	if props == nil {
		props = map[string]any{}
	}
	return &sdk.Tool{
		Name: t.name, Title: t.title, Description: t.description,
		InputSchema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false},
		Annotations: &sdk.ToolAnnotations{Title: t.title, ReadOnlyHint: t.readOnly, DestructiveHint: &t.destructive, OpenWorldHint: &no},
	}
}

func (t tool) handler(call caller) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return failed("arguments must be a JSON object"), nil
			}
		}
		for _, name := range t.required {
			if v, ok := args[name]; !ok || v == nil || v == "" {
				return failed(name + " is required"), nil
			}
		}
		if err := resolveKeys(ctx, call, args); err != nil {
			return failed(err.Error()), nil
		}
		if t.prepare != nil {
			if err := t.prepare(ctx, call, args); err != nil {
				return failed(err.Error()), nil
			}
		}
		path, err := t.url(args)
		if err != nil {
			return failed(err.Error()), nil
		}
		var body any
		if t.rawBody != "" {
			body = args[t.rawBody]
		} else if t.body != nil {
			fields := map[string]any{}
			for _, name := range t.body {
				if v, ok := args[name]; ok {
					fields[name] = v
				}
			}
			body = fields
		}
		status, out := call(ctx, t.method, path, body)
		if status >= 300 {
			return failed(fmt.Sprintf("%d %s: %s", status, http.StatusText(status), strings.TrimSpace(string(out)))), nil
		}
		text := strings.TrimSpace(string(out))
		if text == "" {
			text = "ok"
		}
		res := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
		var structured any
		if json.Unmarshal(out, &structured) == nil {
			if obj, ok := structured.(map[string]any); ok {
				res.StructuredContent = obj
			}
		}
		return res, nil
	}
}

// url fills {placeholders} (path-escaped) and appends query arguments; arrays repeat the key.
func (t tool) url(args map[string]any) (string, error) {
	path := t.path
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			break
		}
		j := strings.Index(path[i:], "}") + i
		name := path[i+1 : j]
		v, ok := args[name].(string)
		if !ok || v == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		path = path[:i] + url.PathEscape(v) + path[j+1:]
	}
	q := url.Values{}
	for _, name := range t.query {
		switch v := args[name].(type) {
		case nil:
		case []any:
			for _, item := range v {
				q.Add(name, fmt.Sprint(item))
			}
		default:
			q.Set(name, fmt.Sprint(v))
		}
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path, nil
}

func failed(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
}

// resolveKeys lets every tool take ticket keys (TL-123) wherever it takes ticket ids: ticket_id,
// parent_id, duplicate_of and ticket_ids are swapped for ids through GET /api/ticket-keys/{key}.
func resolveKeys(ctx context.Context, call caller, args map[string]any) error {
	resolve := func(v any) (any, error) {
		key, ok := v.(string)
		if !ok || !tickets.IsKey(key) {
			return v, nil
		}
		status, out := call(ctx, http.MethodGet, "/api/ticket-keys/"+url.PathEscape(key), nil)
		var t struct {
			ID string `json:"id"`
		}
		if status != http.StatusOK || json.Unmarshal(out, &t) != nil || t.ID == "" {
			return nil, fmt.Errorf("no ticket %s", key)
		}
		return t.ID, nil
	}
	for _, name := range []string{"ticket_id", "parent_id", "duplicate_of"} {
		if v, ok := args[name]; ok {
			id, err := resolve(v)
			if err != nil {
				return err
			}
			args[name] = id
		}
	}
	if list, ok := args["ticket_ids"].([]any); ok {
		for i, v := range list {
			id, err := resolve(v)
			if err != nil {
				return err
			}
			list[i] = id
		}
	}
	return nil
}

// Prompt is what an agent is told about one tool.
type Prompt struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

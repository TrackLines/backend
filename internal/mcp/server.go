// Package mcp serves TrackLines over the Model Context Protocol (Streamable HTTP, stateless), so
// bots (Claude, Codex, …) work with the same data as the web app. Every tool is a REST route
// called in-process through the full handler chain, with the caller's own headers: same auth, org
// scoping, validation and errors as REST, and nothing to keep in sync.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// tool maps one MCP tool onto a REST route. {name} placeholders in path are filled from the
// arguments; query and body list the arguments sent as query parameters or JSON body fields.
type tool struct {
	name, title, description string
	method, path             string
	query, body              []string
	props                    map[string]any // JSON Schema properties
	required                 []string
	readOnly, destructive    bool
	prepare                  func(ctx context.Context, call caller, args map[string]any) error // optional: fill args before the call
}

// caller runs an API request as the MCP client.
type caller func(ctx context.Context, method, path string, body any) (int, []byte)

// Handler is the /mcp endpoint. api is the whole REST handler chain (auth middleware included).
func Handler(api http.Handler, version string) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		s := sdk.NewServer(&sdk.Implementation{Name: "tracklines", Title: "TrackLines", Version: version}, &sdk.ServerOptions{
			Instructions: "TrackLines boards, backlog and tickets. Work is organised as projects → boards (one per team) → columns → tickets; " +
				"the last column of a board is Done. Typical flow: list_projects, get_board or list_backlog, claim_ticket, move_ticket, add_comment.",
		})
		call := requester(api, r.Header)
		for _, t := range tools {
			s.AddTool(t.spec(), t.handler(call))
		}
		return s
	}, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

// requester calls the REST API in-process with the MCP request's auth headers.
func requester(api http.Handler, header http.Header) caller {
	return func(ctx context.Context, method, path string, body any) (int, []byte) {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequestWithContext(ctx, method, path, &buf)
		for _, h := range []string{"Authorization", "X-Request-Id"} {
			if v := header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		req.Header.Set("Content-Type", "application/json")
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
		if t.body != nil {
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

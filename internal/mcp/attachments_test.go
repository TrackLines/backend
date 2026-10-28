package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tracklines/backend/internal/attachments"
)

func named(t *testing.T, name string) tool {
	t.Helper()
	for _, c := range tools {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("%s is not registered", name)
	return tool{}
}

func run(t *testing.T, tl tool, call caller, args string) *sdk.CallToolResult {
	t.Helper()
	res, err := tl.handler(call)(context.Background(), &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Arguments: json.RawMessage(args)}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TL-712: agents attach files through the same upload route as the TUI, as the file's own bytes and type
func TestAddAttachmentTool(t *testing.T) {
	add := named(t, "add_attachment")
	const ticket = "00000000-0000-0000-0000-000000000001"
	var got []string
	call := func(_ context.Context, method, path string, body any) (int, []byte) {
		f, ok := body.(file)
		if !ok {
			t.Fatalf("body %T, want the file", body)
		}
		got = append(got, method+" "+path+" "+f.contentType+" "+string(f.data))
		return http.StatusCreated, []byte(`{"id":"a1","name":"run.log"}`)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("log lines"))
	for _, args := range []string{
		`{"ticket_id":"` + ticket + `","name":"run.log","content_type":"text/plain","content_base64":"` + enc + `"}`,
		`{"ticket_id":"` + ticket + `","name":"run.log","content_base64":"data:text/plain;base64,` + enc[:4] + `\n` + enc[4:] + `"}`, // a data: URL, wrapped
	} {
		if res := run(t, add, call, args); res.IsError {
			t.Fatalf("add_attachment: %+v", res.Content[0])
		}
	}
	want := []string{
		"POST /api/tickets/" + ticket + "/attachments/upload?name=run.log text/plain log lines",
		"POST /api/tickets/" + ticket + "/attachments/upload?name=run.log application/octet-stream log lines",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// refused before anything is sent: not base64, over 10 MB
	got = nil
	big := base64.StdEncoding.EncodeToString(make([]byte, attachments.MaxFileBytes+1))
	for name, args := range map[string]string{
		"not base64": `{"ticket_id":"` + ticket + `","name":"x","content_base64":"%%%"}`,
		"too big":    `{"ticket_id":"` + ticket + `","name":"x","content_base64":"` + big + `"}`,
	} {
		if res := run(t, add, call, args); !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(got) != 0 {
		t.Fatalf("sent a refused file: %v", got)
	}
	// the endpoint's refusals (a done ticket, a read-only key, too many files) come back as tool errors
	refused := func(context.Context, string, string, any) (int, []byte) {
		return http.StatusConflict, []byte("ticket is done")
	}
	if res := run(t, add, refused, `{"ticket_id":"`+ticket+`","name":"x","content_base64":"`+enc+`"}`); !res.IsError {
		t.Fatal("a refused upload looked like it worked")
	}
}

func TestRemoveAttachmentTool(t *testing.T) {
	remove := named(t, "remove_attachment")
	if !remove.destructive {
		t.Fatal("remove_attachment should be marked destructive")
	}
	var got string
	call := func(_ context.Context, method, path string, _ any) (int, []byte) {
		got = method + " " + path
		return http.StatusNoContent, nil
	}
	if res := run(t, remove, call, `{"attachment_id":"a1"}`); res.IsError || got != "DELETE /api/attachments/a1" {
		t.Fatalf("remove_attachment: %s %+v", got, res)
	}
}

// the file reaches the API as its own bytes, with its own type and length, not wrapped in JSON
func TestRequesterSendsFilesRaw(t *testing.T) {
	var body, contentType string
	var length int64
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, contentType, length = string(b), r.Header.Get("Content-Type"), r.ContentLength
		w.WriteHeader(http.StatusCreated)
	})
	call := requester(api, http.Header{"Authorization": {"Bearer tl_x"}})
	if status, _ := call(context.Background(), http.MethodPost, "/api/x", file{data: []byte("PNG…"), contentType: "image/png"}); status != http.StatusCreated {
		t.Fatalf("status %d", status)
	}
	if body != "PNG…" || contentType != "image/png" || length != int64(len("PNG…")) {
		t.Fatalf("got %q %q %d", body, contentType, length)
	}
}

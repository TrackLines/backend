package retention

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TL-540: an import's file goes up through UploadThing's REST API: a signed upload, then the file.
func TestUpload(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v7/prepareUpload":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if r.Header.Get("x-uploadthing-api-key") != "sk_test" || in["fileName"] != "plan.pdf" || in["fileSize"] != float64(5) || in["fileType"] != "application/pdf" {
				t.Errorf("prepare: %v %v", r.Header, in)
			}
			json.NewEncoder(w).Encode(map[string]string{"key": "abc123", "url": srv.URL + "/ingest/abc123?signature=x"})
		case r.Method == http.MethodPut && r.URL.Path == "/ingest/abc123":
			f, h, err := r.FormFile("file")
			if err != nil || h.Filename != "plan.pdf" || r.Header.Get("Range") != "bytes=0-" {
				t.Errorf("upload: %v %v", err, r.Header)
				w.WriteHeader(400)
				return
			}
			b, _ := io.ReadAll(f)
			if string(b) != "hello" {
				t.Errorf("body %q", b)
			}
			json.NewEncoder(w).Encode(map[string]string{"ufsUrl": "https://app1.ufs.sh/f/abc123"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	u := UploadThing{Token: base64.StdEncoding.EncodeToString([]byte(`{"apiKey":"sk_test","appId":"app1"}`)), BaseURL: srv.URL, Client: srv.Client()}
	key, url, err := u.Upload(context.Background(), "plan.pdf", "application/pdf", []byte("hello"))
	if err != nil || key != "abc123" || url != "https://app1.ufs.sh/f/abc123" {
		t.Fatalf("upload: %q %q %v", key, url, err)
	}
	u.BaseURL = srv.URL + "/nope"
	if _, _, err := u.Upload(context.Background(), "plan.pdf", "application/pdf", []byte("hello")); err == nil {
		t.Fatal("a failed prepare succeeded")
	}
}

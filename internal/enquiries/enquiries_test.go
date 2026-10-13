package enquiries

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Contact-form messages reach ChewedFeed HQ with Tracklines' enquiry key; bots, bad input and a
// missing key never get there.
func TestEnquiries(t *testing.T) {
	var got []map[string]string
	var keys []string
	status := http.StatusCreated
	hq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/customer-enquiries" || r.Method != http.MethodPost {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got, keys = append(got, body), append(keys, r.Header.Get("X-Enquiry-Key"))
		w.WriteHeader(status)
	}))
	defer hq.Close()
	h := NewSystem(hq.URL+"/", "enq_tracklines")
	send := func(h System, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Create(rec, httptest.NewRequest("POST", "/api/enquiries", strings.NewReader(body)))
		return rec
	}

	if rec := send(h, `{"name":" Ava ","email":"ava@example.com","company":"Acme","message":"Delete my data, please","source_path":"/privacy"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	if len(got) != 1 || keys[0] != "enq_tracklines" || got[0]["name"] != "Ava" || got[0]["email"] != "ava@example.com" ||
		got[0]["company"] != "Acme" || got[0]["message"] != "Delete my data, please" || got[0]["source_path"] != "/privacy" {
		t.Fatalf("forwarded: %v %v", got, keys)
	}

	// refused before reaching HQ
	for body, want := range map[string]int{
		`{"name":"Bot","email":"b@x.io","message":"buy","website":"http://spam"}`:       http.StatusNoContent, // honeypot: pretend
		`{"name":"","email":"a@x.io","message":"hi"}`:                                   http.StatusBadRequest,
		`{"name":"Ava","email":"a@x.io","message":"  "}`:                                http.StatusBadRequest,
		`{"name":"Ava","email":"not an email","message":"hi"}`:                          http.StatusBadRequest,
		`{"name":"Ava","email":"Ava <a@x.io>","message":"hi"}`:                          http.StatusBadRequest,
		`{"name":"Ava","email":"a@x.io","message":"` + strings.Repeat("x", 5001) + `"}`: http.StatusBadRequest,
	} {
		if rec := send(h, body); rec.Code != want {
			t.Errorf("%.60s: %d, want %d", body, rec.Code, want)
		}
	}
	if len(got) != 1 {
		t.Fatalf("refused enquiries reached HQ: %d", len(got))
	}

	// HQ down or refusing: the sender is told to try again (not that it was sent)
	status = http.StatusUnauthorized
	if rec := send(h, `{"name":"Ava","email":"a@x.io","message":"hi"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("HQ refused: %d", rec.Code)
	}
	// not configured: refused, nothing sent
	if rec := send(NewSystem(hq.URL, ""), `{"name":"Ava","email":"a@x.io","message":"hi"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no key: %d", rec.Code)
	}
	if len(got) != 2 {
		t.Fatalf("calls to HQ: %d", len(got))
	}
}

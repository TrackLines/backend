package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendBatchesThroughResend(t *testing.T) {
	var batches [][]map[string]any
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emails/batch" || r.Header.Get("Authorization") != "Bearer re_test" {
			t.Errorf("request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if fail {
			http.Error(w, `{"message":"domain not verified"}`, http.StatusForbidden)
			return
		}
		var batch []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		batches = append(batches, batch)
	}))
	defer srv.Close()
	s := Sender{Key: "re_test", From: "TrackLin.es <updates@example.test>", URL: srv.URL}

	msgs := make([]Message, 101)
	for i := range msgs {
		msgs[i] = Message{To: "a@example.test", Subject: "s", Text: "t", Headers: map[string]string{"List-Unsubscribe": "<https://x>"}}
	}
	if err := s.Send(context.Background(), msgs...); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != 100 || len(batches[1]) != 1 {
		t.Fatalf("batches: %d", len(batches))
	}
	got := batches[1][0]
	if got["from"] != s.From || got["to"].([]any)[0] != "a@example.test" || got["headers"].(map[string]any)["List-Unsubscribe"] != "<https://x>" {
		t.Fatalf("message: %v", got)
	}

	fail = true
	if err := s.Send(context.Background(), msgs[0]); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a refused send should be an error: %v", err)
	}

	// off without a key: nothing is sent, and it's not an error
	if off := (Sender{From: "x", URL: srv.URL}); off.On() || off.Send(context.Background(), msgs[0]) != nil {
		t.Fatal("a sender without a key should be off")
	}
}

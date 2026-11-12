package integrations

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/tracklines/backend/internal/notifications"
)

func TestFCM(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	var tokens atomic.Int32
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			tok, err := jwt.ParseSigned(r.Form.Get("assertion"), []jose.SignatureAlgorithm{jose.RS256})
			var claims map[string]any
			if err != nil || tok.Claims(&key.PublicKey, &claims) != nil || claims["scope"] != fcmScope || claims["iss"] != "fcm@p.iam.gserviceaccount.com" {
				t.Errorf("assertion: %v %v", err, claims)
			}
			tokens.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"at-1","expires_in":3600}`))
		case r.URL.Path == "/v1/projects/tracklines-test/messages:send":
			if r.Header.Get("Authorization") != "Bearer at-1" {
				t.Errorf("auth %q", r.Header.Get("Authorization"))
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &last)
			switch {
			case strings.Contains(string(body), `"token":"gone"`):
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`))
			case strings.Contains(string(body), `"token":"bad"`):
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"status":"INVALID_ARGUMENT","message":"The registration token is not a valid FCM registration token"}}`))
			case strings.Contains(string(body), `"token":"busy"`):
				w.WriteHeader(http.StatusServiceUnavailable)
			default:
				_, _ = w.Write([]byte(`{"name":"projects/tracklines-test/messages/1"}`))
			}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	if f, err := NewFCM(""); f != nil || err != nil {
		t.Fatalf("no credentials: %v %v", f, err)
	}
	if _, err := NewFCM(`{"client_email":"x"}`); err == nil {
		t.Fatal("bad credentials accepted")
	}
	creds, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "fcm@p.iam.gserviceaccount.com", "private_key_id": "k1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": srv.URL + "/token", "project_id": "tracklines-test"})
	f, err := NewFCM(string(creds))
	if err != nil || f == nil {
		t.Fatalf("NewFCM: %v", err)
	}
	f.API, f.Client = srv.URL, srv.Client()
	ctx := context.Background()
	m := notifications.Message{Title: "TrackLin.es", Body: "Bo assigned you TL-1: Fix login", URL: "/tickets/t1", Tag: "assigned:TL-1", Kind: "assigned"}

	if gone, err := f.SendDevice(ctx, "ava-phone", m); gone || err != nil {
		t.Fatalf("send: %v %v", gone, err)
	}
	msg := last["message"].(map[string]any)
	data := msg["data"].(map[string]any)
	if data["url"] != "/tickets/t1" || data["kind"] != "assigned" || data["body"] != m.Body || msg["android"].(map[string]any)["priority"] != "high" {
		t.Fatalf("message: %v", msg)
	}
	aps := msg["apns"].(map[string]any)["payload"].(map[string]any)["aps"].(map[string]any)
	if aps["alert"].(map[string]any)["body"] != m.Body || aps["thread-id"] != "assigned" || data["urgent"] != "false" || aps["interruption-level"] != nil {
		t.Fatalf("apns: %v", aps)
	}
	// urgent (TL-882): the apps are told, and iOS shows it time-sensitive
	urgent := m
	urgent.Urgent = true
	if _, err := f.SendDevice(ctx, "ava-phone", urgent); err != nil {
		t.Fatal(err)
	}
	msg = last["message"].(map[string]any)
	aps = msg["apns"].(map[string]any)["payload"].(map[string]any)["aps"].(map[string]any)
	if msg["data"].(map[string]any)["urgent"] != "true" || aps["interruption-level"] != "time-sensitive" {
		t.Fatalf("urgent: %v", msg)
	}
	for tok, wantGone := range map[string]bool{"gone": true, "bad": true} {
		if gone, err := f.SendDevice(ctx, tok, m); gone != wantGone || err != nil {
			t.Fatalf("%s: gone %v err %v", tok, gone, err)
		}
	}
	if gone, err := f.SendDevice(ctx, "busy", m); gone || err == nil {
		t.Fatalf("busy: gone %v err %v (should be kept and retried another time)", gone, err)
	}
	if tokens.Load() != 1 {
		t.Fatalf("access token fetched %d times; it should be reused", tokens.Load())
	}
	// near expiry it's fetched again
	f.now = func() time.Time { return time.Now().Add(time.Hour) }
	_, _ = f.SendDevice(ctx, "ava-phone", m)
	if tokens.Load() != 2 {
		t.Fatalf("expired token reused")
	}
}

package apple

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// fakeApple checks each call's client secret, then answers /auth/token with tokenStatus.
func fakeApple(t *testing.T, key *ecdsa.PrivateKey, tokenStatus int, revoked *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "es.tracklin.ios" {
			t.Errorf("client_id = %q", r.Form.Get("client_id"))
		}
		claims := jwt.RegisteredClaims{}
		tok, err := jwt.ParseWithClaims(r.Form.Get("client_secret"), &claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience("https://appleid.apple.com"), jwt.WithIssuer("TEAM"), jwt.WithSubject("es.tracklin.ios"))
		if err != nil || tok.Header["kid"] != "KEY" {
			t.Errorf("client secret: %v, kid %v", err, tok.Header["kid"])
		}
		switch r.URL.Path {
		case "/auth/token":
			if r.Form.Get("code") != "the-code" || r.Form.Get("grant_type") != "authorization_code" {
				t.Errorf("token form: %v", r.Form)
			}
			w.WriteHeader(tokenStatus)
			_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r"}`))
		case "/auth/revoke":
			*revoked = r.Form.Get("token_type_hint") + ":" + r.Form.Get("token")
		}
	}))
}

func revoker(t *testing.T, base string, pemKey string) *Revoker {
	t.Helper()
	a, err := New("TEAM", "KEY", "es.tracklin.ios", strings.ReplaceAll(pemKey, "\n", `\n`)) // as an env var holds it
	if err != nil || a == nil {
		t.Fatalf("New: %v", err)
	}
	a.BaseURL = base
	return a
}

func TestRevokeSwapsTheCodeAndRevokesTheRefreshToken(t *testing.T) {
	key, pemKey := testKey(t)
	var revoked string
	srv := fakeApple(t, key, http.StatusOK, &revoked)
	defer srv.Close()
	a := revoker(t, srv.URL, pemKey)
	a.Key = key

	rec := httptest.NewRecorder()
	a.Handle(rec, httptest.NewRequest(http.MethodPost, "/api/me/apple/revoke", strings.NewReader(`{"code":"the-code"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if revoked != "refresh_token:r" {
		t.Fatalf("revoked %q", revoked)
	}
}

func TestRefusedCodeIsABadRequest(t *testing.T) {
	key, pemKey := testKey(t)
	var revoked string
	srv := fakeApple(t, key, http.StatusBadRequest, &revoked)
	defer srv.Close()
	rec := httptest.NewRecorder()
	revoker(t, srv.URL, pemKey).Handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"code":"the-code"}`)))
	if rec.Code != http.StatusBadRequest || revoked != "" {
		t.Fatalf("status %d, revoked %q", rec.Code, revoked)
	}
}

func TestMissingCodeIsABadRequest(t *testing.T) {
	_, pemKey := testKey(t)
	rec := httptest.NewRecorder()
	revoker(t, "http://unused", pemKey).Handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestUnconfiguredAnswers503(t *testing.T) {
	a, err := New("TEAM", "", "es.tracklin.ios", "")
	if a != nil || err != nil {
		t.Fatalf("New with missing settings = %v, %v", a, err)
	}
	rec := httptest.NewRecorder()
	a.Handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"code":"x"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestBadKeyIsAnError(t *testing.T) {
	if _, err := New("TEAM", "KEY", "es.tracklin.ios", "not a key"); err == nil {
		t.Fatal("want an error")
	}
}

func TestSecretExpiresSoon(t *testing.T) {
	_, pemKey := testKey(t)
	a := revoker(t, "", pemKey)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	s, err := a.secret()
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.RegisteredClaims{}
	_, _, err = jwt.NewParser().ParseUnverified(s, &claims)
	if err != nil || claims.ExpiresAt.Sub(now) > 10*time.Minute {
		t.Fatalf("exp %v, err %v", claims.ExpiresAt, err)
	}
}

// Package apple ends a person's Sign in with Apple grant when they delete their account (TL-857,
// for TL-372): Apple's guideline 5.1.1(v), which Clerk's user deletion doesn't do.
package apple

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tracklines/backend/internal/httpx"
)

// Revoker talks to Apple as the app. A nil one isn't configured.
type Revoker struct {
	TeamID, KeyID, ClientID string
	Key                     *ecdsa.PrivateKey
	BaseURL                 string // https://appleid.apple.com; a fake one in tests
	Client                  *http.Client
	Now                     func() time.Time
}

// New is nil (and no error) when any setting is missing, so the route answers 503.
// key is the .p8 file's PEM; newlines as \n are fine.
func New(teamID, keyID, clientID, key string) (*Revoker, error) {
	if teamID == "" || keyID == "" || clientID == "" || key == "" {
		return nil, nil
	}
	block, _ := pem.Decode([]byte(strings.ReplaceAll(strings.TrimSpace(key), `\n`, "\n")))
	if block == nil {
		return nil, errors.New("apple: private key isn't PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apple: parse private key: %w", err)
	}
	ec, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apple: private key isn't an EC key")
	}
	return &Revoker{TeamID: teamID, KeyID: keyID, ClientID: clientID, Key: ec,
		BaseURL: "https://appleid.apple.com", Client: &http.Client{Timeout: 15 * time.Second}, Now: time.Now}, nil
}

// secret is the client secret Apple wants: a short-lived ES256 JWT signed with the key.
func (a *Revoker) secret() (string, error) {
	now := a.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.RegisteredClaims{
		Issuer:    a.TeamID,
		Subject:   a.ClientID,
		Audience:  jwt.ClaimStrings{"https://appleid.apple.com"},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	})
	t.Header["kid"] = a.KeyID
	return t.SignedString(a.Key)
}

// ErrRefused: Apple turned the code down (used, expired, or for another app).
var ErrRefused = errors.New("apple refused the authorization code")

// Revoke swaps a fresh authorization code from the app for a token, and revokes it.
func (a *Revoker) Revoke(ctx context.Context, code string) error {
	secret, err := a.secret()
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	status, err := a.post(ctx, "/auth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}}, secret, &tok)
	if err != nil {
		return err
	}
	if status == http.StatusBadRequest {
		return ErrRefused
	}
	if status != http.StatusOK {
		return fmt.Errorf("apple token: status %d", status)
	}
	token, hint := tok.RefreshToken, "refresh_token"
	if token == "" {
		token, hint = tok.AccessToken, "access_token"
	}
	status, err = a.post(ctx, "/auth/revoke", url.Values{"token": {token}, "token_type_hint": {hint}}, secret, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("apple revoke: status %d", status)
	}
	return nil
}

func (a *Revoker) post(ctx context.Context, path string, form url.Values, secret string, into any) (int, error) {
	form.Set("client_id", a.ClientID)
	form.Set("client_secret", secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := a.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if into != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			return 0, fmt.Errorf("apple %s: %w", path, err)
		}
	}
	return res.StatusCode, nil
}

// Handle: POST /api/me/apple/revoke {"code": "<authorization code>"}, before the app deletes the
// account. 204 when revoked; 400 when Apple refuses the code; 503 when the server has no Apple key.
func (a *Revoker) Handle(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		httpx.Error(w, r, http.StatusServiceUnavailable, httpx.Code("apple_unconfigured", "Sign in with Apple revocation isn't set up"))
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("missing_code", "code is required"))
		return
	}
	switch err := a.Revoke(r.Context(), body.Code); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrRefused):
		httpx.Error(w, r, http.StatusBadRequest, httpx.Code("apple_refused", err.Error()))
	default:
		httpx.Error(w, r, http.StatusBadGateway, err)
	}
}

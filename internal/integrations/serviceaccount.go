package integrations

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
)

// serviceAccount is our Google Cloud service account (the key JSON Google downloads), shared by the
// Chat app and Google Calendar (TL-405).
type serviceAccount struct {
	email, keyID, tokenURL, clientID string
	key                              *rsa.PrivateKey
}

func parseServiceAccount(credentials string) (*serviceAccount, error) {
	var sa struct {
		Email    string `json:"client_email"`
		KeyID    string `json:"private_key_id"`
		Key      string `json:"private_key"`
		TokenURI string `json:"token_uri"`
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(credentials), &sa); err != nil {
		return nil, fmt.Errorf("credentials aren't a service account key (JSON): %w", err)
	}
	block, _ := pem.Decode([]byte(sa.Key))
	if block == nil || sa.Email == "" {
		return nil, errors.New("credentials have no client_email or private_key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key isn't RSA")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &serviceAccount{email: sa.Email, keyID: sa.KeyID, tokenURL: sa.TokenURI, clientID: sa.ClientID, key: key}, nil
}

// Package secrets seals credentials we store for orgs (integration tokens) with AES-256-GCM under
// the server's key (TRACKLINES_SECRET_KEY, 32 bytes base64), so a database dump alone doesn't
// leak them.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

var ErrNoKey = errors.New("TRACKLINES_SECRET_KEY is not set (32 random bytes, base64)")

// Box seals and opens secrets; the zero value (no key) refuses both, so integrations stay off.
type Box struct{ aead cipher.AEAD }

// New builds a Box from a base64 32-byte key; an empty key gives a Box that refuses (ErrNoKey).
func New(key string) (Box, error) {
	if key == "" {
		return Box{}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return Box{}, errors.New("TRACKLINES_SECRET_KEY must be 32 bytes, base64")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return Box{}, err
	}
	aead, err := cipher.NewGCM(block)
	return Box{aead: aead}, err
}

func (b Box) Ready() bool { return b.aead != nil }

// Seal returns nonce || ciphertext for values that do not have an external identity.
func (b Box) Seal(plain string) ([]byte, error) {
	return b.seal(plain, nil)
}

// SealWithAAD binds ciphertext to its owning record. The same associated data must be
// supplied to OpenWithAAD; changing the org or provider makes authentication fail.
func (b Box) SealWithAAD(plain, associatedData string) ([]byte, error) {
	return b.seal(plain, []byte(associatedData))
}

func (b Box) seal(plain string, associatedData []byte) ([]byte, error) {
	if b.aead == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plain), associatedData), nil
}

func (b Box) Open(sealed []byte) (string, error) {
	return b.open(sealed, nil)
}

// OpenWithAAD opens a value sealed with SealWithAAD.
func (b Box) OpenWithAAD(sealed []byte, associatedData string) (string, error) {
	return b.open(sealed, []byte(associatedData))
}

// OpenLegacy opens values sealed before record-bound associated data was introduced.
// Restrict use to the one-time re-seal migration.
func (b Box) OpenLegacy(sealed []byte) (string, error) {
	return b.Open(sealed)
}

func (b Box) open(sealed, associatedData []byte) (string, error) {
	if b.aead == nil {
		return "", ErrNoKey
	}
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed secret is too short")
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], associatedData)
	return string(plain), err
}

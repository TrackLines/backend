package secrets

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func TestBox(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	b, err := New(key)
	if err != nil || !b.Ready() {
		t.Fatal(err)
	}
	sealed, err := b.Seal("bfx_secret")
	if err != nil || bytes.Contains(sealed, []byte("bfx_secret")) {
		t.Fatalf("seal: %v", err)
	}
	if again, _ := b.Seal("bfx_secret"); bytes.Equal(again, sealed) {
		t.Fatal("same plaintext sealed twice must differ (random nonce)")
	}
	if plain, err := b.Open(sealed); err != nil || plain != "bfx_secret" {
		t.Fatalf("open: %q %v", plain, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("tampered secret opened")
	}
	other, _ := New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	good, _ := b.Seal("x")
	if _, err := other.Open(good); err == nil {
		t.Fatal("opened with the wrong key")
	}
	off, _ := New("")
	if _, err := off.Seal("x"); !errors.Is(err, ErrNoKey) || off.Ready() {
		t.Fatal("no key must refuse")
	}
	if _, err := New("short"); err == nil {
		t.Fatal("bad key accepted")
	}
}

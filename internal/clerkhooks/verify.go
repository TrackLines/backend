package clerkhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

const tolerance = 5 * time.Minute

// Verify checks a Svix-signed delivery (how Clerk sends webhooks): svix-signature holds one or more
// space-separated "v1,<base64>" values of HMAC-SHA256(key, "<svix-id>.<svix-timestamp>.<body>"),
// where key is the base64 part of the whsec_… secret. Stale timestamps are rejected (replay).
func Verify(payload []byte, id, timestamp, signatures, secret string, now time.Time) error {
	if secret == "" {
		return errors.New("webhook secret is not configured")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return errors.New("webhook secret is not valid base64")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if id == "" || err != nil {
		return errors.New("missing svix-id or svix-timestamp")
	}
	if age := now.Sub(time.Unix(ts, 0)); age > tolerance || age < -tolerance {
		return errors.New("signature timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(payload)
	expected := mac.Sum(nil)
	for _, sig := range strings.Fields(signatures) {
		version, value, _ := strings.Cut(sig, ",")
		if got, err := base64.StdEncoding.DecodeString(value); version == "v1" && err == nil && hmac.Equal(got, expected) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

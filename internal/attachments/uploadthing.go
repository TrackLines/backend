package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// UploadThing deletes stored files. Token is UPLOADTHING_TOKEN (base64 JSON with apiKey).
// ponytail: only deleteFiles is needed; uploads go browser → UploadThing via the Next.js route.
type UploadThing struct {
	Token   string
	BaseURL string // defaults to https://api.uploadthing.com
	Client  *http.Client
}

func (u UploadThing) apiKey() (string, error) {
	raw, err := base64.StdEncoding.DecodeString(u.Token)
	if err != nil {
		return "", fmt.Errorf("decode UPLOADTHING_TOKEN: %w", err)
	}
	var t struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.APIKey == "" {
		return "", fmt.Errorf("UPLOADTHING_TOKEN has no apiKey")
	}
	return t.APIKey, nil
}

// DeleteFiles removes files from UploadThing storage by key.
func (u UploadThing) DeleteFiles(ctx context.Context, keys ...string) error {
	key, err := u.apiKey()
	if err != nil {
		return err
	}
	base := u.BaseURL
	if base == "" {
		base = "https://api.uploadthing.com"
	}
	body, _ := json.Marshal(map[string][]string{"fileKeys": keys})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v6/deleteFiles", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-uploadthing-api-key", key)
	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("uploadthing deleteFiles: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("uploadthing deleteFiles: HTTP %d", resp.StatusCode)
	}
	return nil
}

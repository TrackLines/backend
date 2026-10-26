package retention

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// UploadThing deletes stored files. Token is UPLOADTHING_TOKEN (base64 JSON with apiKey).
// Ticket uploads go browser → UploadThing via the Next.js route; imports upload from here.
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

// Upload stores a file the server has (an import's attachments, TL-540): UploadThing's REST API signs
// an upload for it (/v7/prepareUpload), and the file goes up to that URL. It returns the file's key
// and its https://<app>.ufs.sh/f/<key> URL.
func (u UploadThing) Upload(ctx context.Context, name, contentType string, data []byte) (key, url string, err error) {
	apiKey, err := u.apiKey()
	if err != nil {
		return "", "", err
	}
	base := u.BaseURL
	if base == "" {
		base = "https://api.uploadthing.com"
	}
	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	body, _ := json.Marshal(map[string]any{"fileName": name, "fileSize": len(data), "fileType": contentType, "contentDisposition": "inline"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v7/prepareUpload", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-uploadthing-api-key", apiKey)
	var prepared struct{ Key, URL string }
	if err := do(client, req, &prepared); err != nil || prepared.Key == "" || prepared.URL == "" {
		return "", "", fmt.Errorf("uploadthing prepareUpload: %v", err)
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, _ := mw.CreateFormFile("file", name)
	part.Write(data)
	mw.Close()
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, prepared.URL, &form)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Range", "bytes=0-")
	var uploaded struct {
		UfsURL string `json:"ufsUrl"`
	}
	if err := do(client, req, &uploaded); err != nil {
		return "", "", fmt.Errorf("uploadthing upload: %w", err)
	}
	return prepared.Key, uploaded.UfsURL, nil
}

func do(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

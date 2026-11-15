// Package email sends transactional email through Resend (TL-902). Every send goes through here, so
// changing provider touches this file only.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// batchSize: Resend takes up to 100 emails per batch call.
const batchSize = 100

type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
	Headers map[string]string // e.g. List-Unsubscribe
}

// Sender sends through Resend's HTTP API. The zero value is off: On is false and Send does nothing.
type Sender struct {
	Key    string // RESEND_API_KEY (1Password resend_backend.credential)
	From   string // EMAIL_FROM, an address on the domain verified in Resend
	URL    string // tests; https://api.resend.com when empty
	Client *http.Client
}

func (s Sender) On() bool { return s.Key != "" && s.From != "" }

// Send delivers each message to its one recipient, a batch call per 100. It stops at the first failed
// batch; earlier batches have gone.
// ponytail: no retry or queue; add an outbox (like webhooks.Worker) if a missed email starts to matter.
func (s Sender) Send(ctx context.Context, msgs ...Message) error {
	if !s.On() {
		return nil
	}
	type wire struct {
		From    string            `json:"from"`
		To      []string          `json:"to"`
		Subject string            `json:"subject"`
		HTML    string            `json:"html,omitempty"`
		Text    string            `json:"text,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	url, client := s.URL, s.Client
	if url == "" {
		url = "https://api.resend.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	for len(msgs) > 0 {
		n := min(len(msgs), batchSize)
		batch := make([]wire, n)
		for i, m := range msgs[:n] {
			batch[i] = wire{From: s.From, To: []string{m.To}, Subject: m.Subject, HTML: m.HTML, Text: m.Text, Headers: m.Headers}
		}
		msgs = msgs[n:]
		body, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/emails/batch", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.Key)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("email: %w", err)
		}
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		if res.StatusCode >= 300 {
			return fmt.Errorf("email: resend answered %d: %s", res.StatusCode, detail)
		}
	}
	return nil
}

// Package enquiries takes contact-form messages and files them in ChewedFeed HQ's enquiry inbox,
// where every product's enquiries land (the same endpoint the Bugfixes site uses).
package enquiries

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/httpx"
)

// System forwards enquiries to URL (CHEWEDFEED_API_URL) with Key (CHEWEDFEED_ENQUIRY_KEY), which also
// files them under Tracklines. Either empty: enquiries are refused (503).
type System struct {
	URL    string
	Key    string
	Client *http.Client
}

// NewSystem exposes the enquiry handler; the route is declared in internal/service.go.
func NewSystem(url, key string) System { return System{URL: url, Key: key} }

type input struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Company    string `json:"company"`
	Message    string `json:"message"`
	SourcePath string `json:"source_path"`
	Website    string `json:"website"` // honeypot: hidden from people, filled in by bots
}

// limits mirror ChewedFeed's, so a bad field is refused here with a clear message.
var limits = map[string]int{"name": 160, "email": 320, "company": 160, "message": 5000, "source_path": 500}

// Create: POST /api/enquiries {name, email, company?, message, source_path?}. Public; rate limited per address.
func (h System) Create(w http.ResponseWriter, r *http.Request) {
	var in input
	if !httpx.Decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Website) != "" {
		w.WriteHeader(http.StatusNoContent) // a bot: pretend it worked
		return
	}
	fields := map[string]*string{"name": &in.Name, "email": &in.Email, "company": &in.Company, "message": &in.Message, "source_path": &in.SourcePath}
	for name, v := range fields {
		if *v = strings.TrimSpace(*v); len([]rune(*v)) > limits[name] {
			http.Error(w, name+" is too long", http.StatusBadRequest)
			return
		}
	}
	if in.Name == "" || in.Message == "" {
		http.Error(w, "name and message are required", http.StatusBadRequest)
		return
	}
	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email {
		http.Error(w, "a valid email address is required", http.StatusBadRequest)
		return
	}
	if h.URL == "" || h.Key == "" {
		logs.Errorf("enquiries: CHEWEDFEED_API_URL or CHEWEDFEED_ENQUIRY_KEY is not set; enquiry refused")
		http.Error(w, "enquiries aren't available right now", http.StatusServiceUnavailable)
		return
	}
	body, _ := json.Marshal(map[string]string{"name": in.Name, "email": in.Email, "company": in.Company, "message": in.Message, "source_path": in.SourcePath})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(h.URL, "/")+"/api/customer-enquiries", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Enquiry-Key", h.Key)
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		logs.Errorf("enquiries: ChewedFeed unreachable: %v", err)
		http.Error(w, "couldn't send your message; please try again", http.StatusBadGateway)
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logs.Errorf("enquiries: ChewedFeed rejected an enquiry: HTTP %d", resp.StatusCode)
		http.Error(w, "couldn't send your message; please try again", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

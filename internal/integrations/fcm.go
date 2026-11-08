package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/tracklines/backend/internal/notifications"
)

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// FCM sends pushes to the phone apps through Firebase Cloud Messaging's HTTP v1 API (TL-359), with a
// service account that can send messages. iOS goes through it too, once Firebase has the APNs key.
type FCM struct {
	API    string // https://fcm.googleapis.com; a fake one in tests
	Client *http.Client
	now    func() time.Time
	sa     *serviceAccount

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewFCM reads the service account key (FCM_CREDENTIALS). No key: nil, nothing sent to phones.
func NewFCM(credentials string) (*FCM, error) {
	if strings.TrimSpace(credentials) == "" {
		return nil, nil
	}
	sa, err := parseServiceAccount(credentials)
	if err != nil {
		return nil, fmt.Errorf("fcm: %w", err)
	}
	if sa.projectID == "" {
		return nil, errors.New("fcm: credentials have no project_id")
	}
	return &FCM{API: "https://fcm.googleapis.com", Client: externalClient(10 * time.Second), sa: sa}, nil
}

func (f *FCM) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// accessToken is the service account's OAuth token for sending, reused until near expiry.
// ponytail: the same JWT-bearer exchange as ChatApp.accessToken; share it if a third caller appears.
func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.clock()
	if f.token != "" && now.Before(f.tokenExp.Add(-time.Minute)) {
		return f.token, nil
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.sa.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", f.sa.keyID))
	if err != nil {
		return "", err
	}
	assertion, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": f.sa.email, "scope": fcmScope, "aud": f.sa.tokenURL, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}).Serialize()
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.sa.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := f.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("google token: %w", err)
	}
	defer res.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("google token: %s", res.Status)
	}
	f.token, f.tokenExp = out.AccessToken, now.Add(time.Duration(out.ExpiresIn)*time.Second)
	return f.token, nil
}

// fcmMessage is HTTP v1's message. Android gets data only, so the app builds the notification itself:
// its channel per kind and where a tap goes. iOS can't show data alone, so APNs gets an alert too.
type fcmMessage struct {
	Token   string            `json:"token"`
	Data    map[string]string `json:"data"`
	Android struct {
		Priority string `json:"priority"`
	} `json:"android"`
	APNs struct {
		Payload struct {
			APS struct {
				Alert struct {
					Title string `json:"title"`
					Body  string `json:"body"`
				} `json:"alert"`
				Sound    string `json:"sound"`
				ThreadID string `json:"thread-id"`
			} `json:"aps"`
		} `json:"payload"`
	} `json:"apns"`
}

// SendDevice sends m to one app install. gone: FCM says the token is no longer valid (app removed or
// token rotated), so it can be dropped.
func (f *FCM) SendDevice(ctx context.Context, token string, m notifications.Message) (bool, error) {
	access, err := f.accessToken(ctx)
	if err != nil {
		return false, err
	}
	var msg fcmMessage
	msg.Token = token
	msg.Data = map[string]string{"title": m.Title, "body": m.Body, "url": m.URL, "tag": m.Tag, "kind": m.Kind}
	msg.Android.Priority = "high"
	msg.APNs.Payload.APS.Alert.Title, msg.APNs.Payload.APS.Alert.Body = m.Title, m.Body
	msg.APNs.Payload.APS.Sound, msg.APNs.Payload.APS.ThreadID = "default", m.Tag
	body, _ := json.Marshal(map[string]any{"message": msg})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.API+"/v1/projects/"+url.PathEscape(f.sa.projectID)+"/messages:send", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	res, err := f.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return false, nil
	}
	detail, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	// UNREGISTERED comes as 404; a token that was never valid as 400 INVALID_ARGUMENT naming the token
	if res.StatusCode == http.StatusNotFound || bytes.Contains(detail, []byte("UNREGISTERED")) ||
		(res.StatusCode == http.StatusBadRequest && bytes.Contains(detail, []byte("registration token"))) {
		return true, nil
	}
	return false, fmt.Errorf("fcm answered %s", res.Status)
}

// Package live provides short-lived shared board sessions and their event stream.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/valkey-io/valkey-go"
)

const sessionTTL = 12 * time.Hour

var ErrNoSession = errors.New("no live session")

type Participant struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Standup's clock is elapsed time before this run plus now-runningSince when runningSince is set.
type Standup struct {
	People       []Participant `json:"people"`
	Index        int           `json:"index"`
	SpentMS      int64         `json:"spentMs"`
	RunningSince *int64        `json:"runningSince"`
	StartedBy    string        `json:"startedBy"`
}

type Event struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type System struct {
	VK     valkey.Client
	Boards boards.Store
	Now    func() time.Time
	// IsAdmin: is user an admin of org (organizations.Admins.IsAdmin); refinement needs a manager (TL-241)
	IsAdmin func(ctx context.Context, org, user string) (bool, error)
}

func New(vk valkey.Client, db boards.Store) System { return System{VK: vk, Boards: db, Now: time.Now} }

func (s System) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.VK == nil {
		http.Error(w, "live sessions unavailable", http.StatusServiceUnavailable)
		return false
	}
	if _, err := s.Boards.Get(r.Context(), r.PathValue("id"), auth.OrgID(r.Context())); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

func sessionKey(board string) string { return "tracklines:live:session:" + board }
func eventsKey(board string) string  { return "tracklines:live:events:" + board }

func (s System) get(ctx context.Context, board string) (Standup, error) {
	var v Standup
	err := s.VK.Do(ctx, s.VK.B().Get().Key(sessionKey(board)).Build()).DecodeJSON(&v)
	if valkey.IsValkeyNil(err) {
		return Standup{}, ErrNoSession
	}
	return v, err
}

func (s System) save(ctx context.Context, board string, v Standup) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.VK.Do(ctx, s.VK.B().Set().Key(sessionKey(board)).Value(string(raw)).Ex(sessionTTL).Build()).Error()
}

func (s System) publish(ctx context.Context, board, eventType string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	stamp := s.Now().UnixMilli()
	e := Event{ID: stamp, Type: eventType, Payload: raw}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err = s.VK.Do(ctx, s.VK.B().Set().Key(eventsKey(board)).Value(string(data)).Ex(sessionTTL).Build()).Error(); err != nil {
		return err
	}
	return s.VK.Do(ctx, s.VK.B().Publish().Channel("tracklines:live:board:"+board).Message(string(data)).Build()).Error()
}

func (s System) Start(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	var in struct {
		People []Participant `json:"people"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.People == nil {
		in.People = []Participant{}
	}
	startedBy, _ := auth.UserID(r.Context())
	// starting one mid-standup would make the caller its host (TL-250): people only, and not while
	// someone else's is running and they're still there (it ends itself once its host has gone)
	if auth.ViaAPIKey(r.Context()) {
		http.Error(w, "sign in to run a standup", http.StatusForbidden)
		return
	}
	if h, err := s.hosted(r.Context(), boardScope(standup, r.PathValue("id")), standup); err == nil && h.Host != startedBy {
		http.Error(w, "a standup is already running: join it", http.StatusConflict)
		return
	} else if err != nil && !errors.Is(err, ErrNoSession) {
		http.Error(w, "live session unavailable", 503)
		return
	}
	// opens stopped at the first person: the host starts the clock once everyone's joined (TL-231)
	v := Standup{People: in.People, Index: 0, SpentMS: 0, RunningSince: nil, StartedBy: startedBy}
	if err := s.save(r.Context(), r.PathValue("id"), v); err != nil {
		http.Error(w, "live session unavailable", 503)
		return
	}
	_ = s.publish(r.Context(), r.PathValue("id"), "standup", v)
	// the starter hosts it (TL-225): a fresh hosted session, replacing any left from an earlier standup
	sc := boardScope(standup, r.PathValue("id"))
	_ = s.endHosted(r.Context(), sc, standup)
	if _, err := s.startHosted(r.Context(), sc, standup, startedBy); err != nil {
		logs.Errorf("live: start the hosted standup: %v", err)
	}
	httpx.JSON(w, http.StatusCreated, v)
}

func (s System) Get(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	v, err := s.get(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNoSession) {
		http.Error(w, "no live session", 404)
		return
	}
	if err != nil {
		http.Error(w, "live session unavailable", 503)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

func (s System) Act(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	board := r.PathValue("id")
	v, err := s.get(r.Context(), board)
	if errors.Is(err, ErrNoSession) {
		http.Error(w, "no live session", 404)
		return
	}
	if err != nil {
		http.Error(w, "live session unavailable", 503)
		return
	}
	// only the host runs the standup (TL-235); with no hosted session (its host gone too long), anyone can
	user, _ := auth.UserID(r.Context())
	if h, err := s.hosted(r.Context(), boardScope(standup, board), standup); err == nil && h.Host != user {
		http.Error(w, "only the host can run the standup", http.StatusForbidden)
		return
	} else if err != nil && !errors.Is(err, ErrNoSession) {
		http.Error(w, "live session unavailable", 503)
		return
	}
	var in struct {
		Action string `json:"action"`
		Index  *int   `json:"index"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Action == "end" {
		_ = s.VK.Do(r.Context(), s.VK.B().Del().Key(sessionKey(board)).Build()).Error()
		_ = s.publish(r.Context(), board, "standup-ended", nil)
		_ = s.endHosted(r.Context(), boardScope(standup, board), standup)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	v, ok := transition(v, in.Action, in.Index, s.Now().UnixMilli())
	if !ok {
		http.Error(w, "action must be next, previous, jump, pause, resume or end", 400)
		return
	}
	if err := s.save(r.Context(), board, v); err != nil {
		http.Error(w, "live session unavailable", 503)
		return
	}
	_ = s.publish(r.Context(), board, "standup", v)
	httpx.JSON(w, http.StatusOK, v)
}

func transition(v Standup, action string, index *int, now int64) (Standup, bool) {
	switch action {
	case "next":
		v.Index++
		if v.Index > len(v.People) {
			v.Index = len(v.People)
		}
		v.SpentMS, v.RunningSince = 0, &now
	case "previous":
		v.Index--
		if v.Index < 0 {
			v.Index = 0
		}
		v.SpentMS, v.RunningSince = 0, &now
	case "jump":
		if index == nil {
			return v, false
		}
		v.Index = *index
		if v.Index < 0 {
			v.Index = 0
		}
		if v.Index > len(v.People) {
			v.Index = len(v.People)
		}
		v.SpentMS, v.RunningSince = 0, &now
	case "pause":
		if v.RunningSince != nil {
			v.SpentMS += now - *v.RunningSince
			v.RunningSince = nil
		}
	case "resume":
		if v.RunningSince == nil {
			v.RunningSince = &now
		}
	default:
		return v, false
	}
	return v, true
}

// Stream sends the latest retained state on connect and then fans out Valkey pub/sub events.
func (s System) Stream(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	board := r.PathValue("id")
	s.stream(w, r, eventsKey(board), "tracklines:live:board:"+board)
}

// stream serves one live channel as server-sent events: the retained latest event first (so late
// joiners catch up), then everything published, with a heartbeat to keep proxies from closing it.
func (s System) stream(w http.ResponseWriter, r *http.Request, events, channel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if raw, err := s.VK.Do(r.Context(), s.VK.B().Get().Key(events).Build()).ToString(); err == nil {
		writeSSE(w, raw)
		flusher.Flush()
	}
	msgs := make(chan string, 16)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		_ = s.VK.Receive(ctx, s.VK.B().Subscribe().Channel(channel).Build(), func(m valkey.PubSubMessage) {
			select {
			case msgs <- m.Message:
			default:
			}
		})
	}()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-msgs:
			writeSSE(w, msg)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, raw string) {
	var e Event
	if json.Unmarshal([]byte(raw), &e) != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", strconv.FormatInt(e.ID, 10), strings.TrimSpace(e.Type), raw)
}

// PublishRefinement broadcasts a compact invalidation event after a refinement mutation.
func (s System) PublishRefinement(ctx context.Context, board, action string) error {
	if s.VK == nil {
		return nil
	}
	return s.publish(ctx, board, "refinement", map[string]string{"action": action})
}

// StandupRunning: is a standup running on board right now? false when live sessions are off.
func (s System) StandupRunning(ctx context.Context, board string) bool {
	if s.VK == nil {
		return false
	}
	_, err := s.get(ctx, board)
	return err == nil
}

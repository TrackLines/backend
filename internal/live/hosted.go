package live

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Hosted sessions (TL-221): someone hosts a refinement or standup and others join. What the host has
// open (Focus) is shared, so followers see it without screen sharing; People is who's there now.

const (
	presenceWindow = 60 * time.Second // gone after a minute without a heartbeat (clients send one every 30s)
	hostGrace      = 2 * time.Minute  // a session whose host has been gone this long ends
)

var (
	ErrSessionRunning = errors.New("a session is already running")
	ErrNotHost        = errors.New("only the host can do that")
	ErrNotJoined      = errors.New("they haven't joined the session")
)

// Focus is what the host has open: a ticket (nil when none) and the filters they're using.
type Focus struct {
	TicketID *string             `json:"ticketId"`
	Filters  map[string][]string `json:"filters,omitempty"`
}

// Hosted is a hosted session as everyone sees it.
type Hosted struct {
	Host      string   `json:"host"`
	StartedAt int64    `json:"startedAt"`
	Focus     Focus    `json:"focus"`
	People    []string `json:"people"` // joined and seen in the last minute, host included
}

// scope names one session's keys and channel: kind ("refinement", "standup") on a project or board.
type scope struct{ session, people, events, channel string }

// boardScope is a board's hosted standup: published on the board's channel (which the standup page
// already streams), with its own retained event so it doesn't replace the standup's.
func boardScope(kind, board string) scope {
	return scope{
		session: "tracklines:live:" + kind + ":board:" + board,
		people:  "tracklines:live:" + kind + ":board:" + board + ":people",
		events:  "tracklines:live:events:" + kind + ":board:" + board,
		channel: "tracklines:live:board:" + board,
	}
}

func projectScope(kind, project string) scope {
	return scope{
		session: "tracklines:live:" + kind + ":project:" + project,
		people:  "tracklines:live:" + kind + ":project:" + project + ":people",
		events:  "tracklines:live:events:project:" + project,
		channel: "tracklines:live:project:" + project,
	}
}

// hosted loads the session with who's here now. A session whose host has been gone longer than
// hostGrace is ended (and the end published) and reported as ErrNoSession.
func (s System) hosted(ctx context.Context, sc scope, kind string) (Hosted, error) {
	var h Hosted
	err := s.VK.Do(ctx, s.VK.B().Get().Key(sc.session).Build()).DecodeJSON(&h)
	if valkey.IsValkeyNil(err) {
		return Hosted{}, ErrNoSession
	}
	if err != nil {
		return Hosted{}, err
	}
	now := s.Now()
	seen, err := s.VK.Do(ctx, s.VK.B().Zrangebyscore().Key(sc.people).Min(strconv.FormatInt(now.Add(-presenceWindow).UnixMilli(), 10)).Max("+inf").Build()).AsStrSlice()
	if err != nil {
		return Hosted{}, err
	}
	h.People = seen
	if !slices.Contains(seen, h.Host) {
		last, err := s.VK.Do(ctx, s.VK.B().Zscore().Key(sc.people).Member(h.Host).Build()).AsFloat64()
		if (valkey.IsValkeyNil(err) && now.UnixMilli()-h.StartedAt > hostGrace.Milliseconds()) || (err == nil && now.UnixMilli()-int64(last) > hostGrace.Milliseconds()) {
			_ = s.endHosted(ctx, sc, kind)
			return Hosted{}, ErrNoSession
		}
	}
	return h, nil
}

func (s System) saveHosted(ctx context.Context, sc scope, kind string, h Hosted) (Hosted, error) {
	people := h.People
	h.People = nil
	raw, err := json.Marshal(h)
	if err != nil {
		return h, err
	}
	if err := s.VK.Do(ctx, s.VK.B().Set().Key(sc.session).Value(string(raw)).Ex(sessionTTL).Build()).Error(); err != nil {
		return h, err
	}
	h.People = people
	return h, s.publishTo(ctx, sc, kind+"-session", h)
}

// heartbeat marks user as here now (joining, or still here).
func (s System) heartbeat(ctx context.Context, sc scope, user string) error {
	cmds := valkey.Commands{
		s.VK.B().Zadd().Key(sc.people).ScoreMember().ScoreMember(float64(s.Now().UnixMilli()), user).Build(),
		s.VK.B().Expire().Key(sc.people).Seconds(int64(sessionTTL.Seconds())).Build(),
	}
	for _, r := range s.VK.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return err
		}
	}
	return nil
}

// startHosted makes user the host of a new session; refused while one is running.
func (s System) startHosted(ctx context.Context, sc scope, kind, user string) (Hosted, error) {
	if _, err := s.hosted(ctx, sc, kind); err == nil {
		return Hosted{}, ErrSessionRunning
	} else if !errors.Is(err, ErrNoSession) {
		return Hosted{}, err
	}
	_ = s.VK.Do(ctx, s.VK.B().Del().Key(sc.people).Build()).Error()
	if err := s.heartbeat(ctx, sc, user); err != nil {
		return Hosted{}, err
	}
	return s.saveHosted(ctx, sc, kind, Hosted{Host: user, StartedAt: s.Now().UnixMilli(), People: []string{user}})
}

// joinHosted adds user (or keeps them there: clients call it every 30s) and announces newcomers.
func (s System) joinHosted(ctx context.Context, sc scope, kind, user string) (Hosted, error) {
	h, err := s.hosted(ctx, sc, kind)
	if err != nil {
		return Hosted{}, err
	}
	if err := s.heartbeat(ctx, sc, user); err != nil {
		return Hosted{}, err
	}
	if slices.Contains(h.People, user) {
		return h, nil // still here: nothing new to tell anyone
	}
	h.People = append(h.People, user)
	return h, s.publishTo(ctx, sc, kind+"-session", h)
}

// leaveHosted takes user out; the host leaving without handing over ends the session after hostGrace.
func (s System) leaveHosted(ctx context.Context, sc scope, kind, user string) error {
	if err := s.VK.Do(ctx, s.VK.B().Zrem().Key(sc.people).Member(user).Build()).Error(); err != nil {
		return err
	}
	h, err := s.hosted(ctx, sc, kind)
	if errors.Is(err, ErrNoSession) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.publishTo(ctx, sc, kind+"-session", h)
}

// focusHosted sets what the host has open; only the host can.
func (s System) focusHosted(ctx context.Context, sc scope, kind, user string, f Focus) (Hosted, error) {
	h, err := s.hosted(ctx, sc, kind)
	if err != nil {
		return Hosted{}, err
	}
	if h.Host != user {
		return Hosted{}, ErrNotHost
	}
	h.Focus = f
	return s.saveHosted(ctx, sc, kind, h)
}

// handOver makes to the host. The host can hand over to anyone here; anyone here can take over
// once the host has gone (not seen for a minute).
func (s System) handOver(ctx context.Context, sc scope, kind, user, to string) (Hosted, error) {
	h, err := s.hosted(ctx, sc, kind)
	if err != nil {
		return Hosted{}, err
	}
	hostGone := !slices.Contains(h.People, h.Host)
	if h.Host != user && !(hostGone && to == user && slices.Contains(h.People, user)) {
		return Hosted{}, ErrNotHost
	}
	if !slices.Contains(h.People, to) {
		return Hosted{}, ErrNotJoined
	}
	h.Host = to
	return s.saveHosted(ctx, sc, kind, h)
}

// endHosted ends the session for everyone.
func (s System) endHosted(ctx context.Context, sc scope, kind string) error {
	if err := s.VK.Do(ctx, s.VK.B().Del().Key(sc.session, sc.people).Build()).Error(); err != nil {
		return err
	}
	return s.publishTo(ctx, sc, kind+"-session-ended", nil)
}

// publishTo keeps the latest event (for late joiners) and fans it out on the scope's channel.
func (s System) publishTo(ctx context.Context, sc scope, eventType string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data, err := json.Marshal(Event{ID: s.Now().UnixMilli(), Type: eventType, Payload: raw})
	if err != nil {
		return err
	}
	if err = s.VK.Do(ctx, s.VK.B().Set().Key(sc.events).Value(string(data)).Ex(sessionTTL).Build()).Error(); err != nil {
		return err
	}
	return s.VK.Do(ctx, s.VK.B().Publish().Channel(sc.channel).Message(string(data)).Build()).Error()
}

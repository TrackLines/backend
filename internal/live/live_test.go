package live

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/valkey-io/valkey-go"
)

func TestStandupTransitions(t *testing.T) {
	now := int64(10_000)
	running := int64(2_000)
	v := Standup{People: []Participant{{ID: "a"}, {ID: "b"}}, Index: 0, SpentMS: 500, RunningSince: &running}

	paused, ok := transition(v, "pause", nil, now)
	if !ok || paused.SpentMS != 8_500 || paused.RunningSince != nil {
		t.Fatalf("pause = %#v", paused)
	}
	resumed, ok := transition(paused, "resume", nil, now+100)
	if !ok || resumed.RunningSince == nil || *resumed.RunningSince != now+100 || resumed.SpentMS != 8_500 {
		t.Fatalf("resume = %#v", resumed)
	}
	jump := -1
	review, ok := transition(resumed, "jump", &jump, now+200)
	if !ok || review.Index != 0 || review.SpentMS != 0 || review.RunningSince == nil {
		t.Fatalf("clamped jump = %#v", review)
	}
	end := 99
	review, ok = transition(review, "jump", &end, now+300)
	if !ok || review.Index != len(review.People) {
		t.Fatalf("review jump = %#v", review)
	}
	next, ok := transition(review, "next", nil, now+400)
	if !ok || next.Index != len(next.People) {
		t.Fatalf("next past review = %#v", next)
	}
	if _, ok = transition(v, "jump", nil, now); ok {
		t.Fatal("jump without index must be rejected")
	}
}

func TestTwoSubscribersReceiveSharedEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	container, err := tcvalkey.Run(ctx, "valkey/valkey:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := valkey.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	vk, err := valkey.NewClient(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vk.Close)

	got := []chan Event{make(chan Event, 1), make(chan Event, 1)}
	for i := range got {
		ch := got[i]
		go func() {
			_ = vk.Receive(ctx, vk.B().Subscribe().Channel("tracklines:live:board:b1").Build(), func(m valkey.PubSubMessage) {
				var e Event
				if json.Unmarshal([]byte(m.Message), &e) == nil {
					select {
					case ch <- e:
					default:
					}
				}
			})
		}()
	}
	system := System{VK: vk, Now: func() time.Time { return time.UnixMilli(1234) }}
	deadline := time.After(5 * time.Second)
	for {
		_ = system.PublishRefinement(ctx, "b1", "approve")
		if len(got[0]) > 0 && len(got[1]) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("both subscribers did not receive the event")
		case <-time.After(20 * time.Millisecond):
		}
	}
	for i, ch := range got {
		e := <-ch
		var payload map[string]string
		if e.Type != "refinement" || json.Unmarshal(e.Payload, &payload) != nil || payload["action"] != "approve" {
			t.Fatalf("subscriber %d received %#v", i+1, e)
		}
	}
}

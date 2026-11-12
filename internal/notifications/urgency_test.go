package notifications

import (
	"context"
	"testing"
)

type caught struct{ e Event }

func (c *caught) Notify(_ context.Context, _ string, _ []string, e Event) error { c.e = e; return nil }

// policy2 is asked about the event's type, and its answer travels with the event (TL-882).
func TestUrgent(t *testing.T) {
	for e, want := range map[Event]string{
		{Kind: "billing", Type: "offer"}: "offer",
		{Kind: "billing"}:                "billing",
		{Kind: "finished"}:               "state-change",
		{Kind: "wont_do"}:                "state-change",
		{Kind: "mention"}:                "mention",
	} {
		if got := UrgencyType(e); got != want {
			t.Errorf("%+v: %q, want %q", e, got, want)
		}
	}
	var asked []string
	next := &caught{}
	u := Urgent{Next: next, Check: func(_ context.Context, typ string) bool { asked = append(asked, typ); return typ == "offer" }}
	_ = u.Notify(context.Background(), "o", []string{"user_ava"}, Event{Kind: "billing", Type: "offer"})
	if !next.e.Urgent {
		t.Fatal("offer not urgent")
	}
	_ = u.Notify(context.Background(), "o", []string{"user_ava"}, Event{Kind: "assigned"})
	if next.e.Urgent {
		t.Fatal("assigned urgent")
	}
	_ = u.Notify(context.Background(), "o", nil, Event{Kind: "created"}) // to spaces: nobody to open anything for
	if len(asked) != 2 {
		t.Fatalf("asked: %v", asked)
	}
}

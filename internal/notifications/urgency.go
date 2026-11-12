package notifications

import "context"

// Urgency says whether a notification type is urgent (TL-882); policy2 decides (integrations.UrgentPolicy).
type Urgency func(ctx context.Context, typ string) bool

// Urgent sends through Next with each event's urgency filled in from Check.
type Urgent struct {
	Next  Notifier
	Check Urgency
}

func (u Urgent) Notify(ctx context.Context, org string, to []string, e Event) error {
	if u.Check != nil && len(to) > 0 { // an announcement to spaces has nobody to open anything for
		e.Urgent = u.Check(ctx, UrgencyType(e))
	}
	return u.Next.Notify(ctx, org, to, e)
}

// UrgencyType is what policy2 is asked about: Type when set (the launch offer is "offer"), finished and
// won't do are both a "state-change", anything else its kind (billing, assigned, mention, …).
func UrgencyType(e Event) string {
	switch {
	case e.Type != "":
		return e.Type
	case e.Kind == "finished" || e.Kind == "wont_do":
		return "state-change"
	}
	return e.Kind
}

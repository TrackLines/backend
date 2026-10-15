package tickets

import (
	"context"
	"net/http"
	"strings"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/notifications"
)

// WithNotifier tells people about ticket events (assigned, finished, won't do, review); nil: nobody.
func (h System) WithNotifier(n notifications.Notifier) System { h.notify = n; return h }

// ref names a ticket in a notification: "TL-12: Fix login".
func ref(d *Detail) string {
	if d.Key == "" {
		return d.Title
	}
	return d.Key + ": " + d.Title
}

func (h System) tell(r *http.Request, to []string, kind, text string, d *Detail) {
	if h.notify == nil || d == nil {
		return
	}
	board := ""
	if d.BoardID != nil {
		board = *d.BoardID
	}
	notifications.Send(r.Context(), h.notify, auth.OrgID(r.Context()), to, notifications.Event{
		Kind: kind, Text: text, Actor: auth.ActorID(r.Context()), TicketID: d.ID, BoardID: board,
	})
}

// created tells the project's spaces (Google Chat, TL-288) a ticket was made: on a board or in the backlog.
func (h System) created(r *http.Request, id string) {
	d := h.detail(r.Context(), auth.OrgID(r.Context()), id)
	if d == nil {
		return
	}
	text := "added " + ref(d) + " to the backlog"
	board := ""
	if d.BoardID != nil {
		board = *d.BoardID
		if d.BoardName != nil {
			text = "created " + ref(d) + " on " + *d.BoardName
		}
	}
	notifications.Announce(r.Context(), h.notify, auth.OrgID(r.Context()), notifications.Event{
		Kind: "created", Text: text, Actor: auth.ActorID(r.Context()), TicketID: d.ID, BoardID: board,
	})
}

// detail is the ticket as it is now, for notifications (nil when it can't be read).
func (h System) detail(ctx context.Context, org, id string) *Detail {
	if h.notify == nil {
		return nil
	}
	d, err := h.store.Get(ctx, org, id)
	if err != nil {
		return nil
	}
	return d
}

// finished tells the ticket's creator it's done (people only; their own finish isn't news).
func (h System) finished(r *http.Request, d *Detail, verb string) {
	if d != nil {
		h.tell(r, []string{d.CreatedBy}, "finished", verb+" "+ref(d), d)
	}
}

// reviewers are told an agent's finished ticket waits for them.
func (h System) reviewers(r *http.Request, d *Detail) {
	if h.notify == nil || d == nil || d.BoardID == nil {
		return
	}
	to, err := notifications.Store{DB: h.store.DB}.BoardManagers(r.Context(), auth.OrgID(r.Context()), *d.BoardID)
	if err == nil {
		h.tell(r, to, "review", "finished "+ref(d)+", and it needs your review", d)
	}
}

// People are the org's members by the names they can be @mentioned with: their full name, first
// name, or the part of their email before the @.
func (h System) People(ctx context.Context, org string) ([]notifications.Person, error) {
	members, err := h.organizationAssignees(ctx, org, "")
	if err != nil {
		return nil, err
	}
	out := make([]notifications.Person, 0, len(members))
	for _, m := range members {
		names := []string{m.Label}
		if local, _, ok := strings.Cut(m.Label, "@"); ok {
			names = append(names, local)
		} else if first, _, ok := strings.Cut(m.Label, " "); ok {
			names = append(names, first)
		}
		if m.Username != "" {
			names = append(names, m.Username)
		}
		out = append(out, notifications.Person{ID: m.ID, Names: names})
	}
	return out, nil
}

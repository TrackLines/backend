// Package notifications tells people about things that happened to them in Tracklines: assigned a
// ticket, mentioned, a ticket they raised finished or won't be done, agent work waiting for their
// review, a planned sprint needing their approval. Internal for now (an inbox); external channels
// (notfy.dev, email, chat) are another Notifier later, without touching the callers.
package notifications

import (
	"context"
	"regexp"
	"strings"

	"github.com/bugfixes/go-bugfixes/logs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one thing that happened. Text reads after the actor's name, e.g. "assigned you TL-12: Fix login".
type Event struct {
	Kind     string
	Text     string
	Actor    string // user id or agent name; never told about their own action
	TicketID string // optional
	BoardID  string // optional
}

// Notifier tells users (Clerk user ids) in org about e.
type Notifier interface {
	Notify(ctx context.Context, org string, to []string, e Event) error
}

// Send notifies through n, never failing the caller's request: a nil n is a no-op, errors are logged.
func Send(ctx context.Context, n Notifier, org string, to []string, e Event) {
	if n == nil || len(to) == 0 {
		return
	}
	if err := n.Notify(ctx, org, to, e); err != nil {
		logs.Errorf("notifications: %s: %v", e.Kind, err)
	}
}

// Announce is an event for spaces, not people (TL-288: "created TL-12 on Backend" in the team's Google
// Chat): nobody's inbox or device gets it; channels that post to a space (GoogleChat) do.
func Announce(ctx context.Context, n Notifier, org string, e Event) {
	if n == nil {
		return
	}
	if err := n.Notify(ctx, org, nil, e); err != nil {
		logs.Errorf("notifications: %s: %v", e.Kind, err)
	}
}

// Store keeps the inbox in Postgres; it's the internal Notifier.
type Store struct{ DB *pgxpool.Pool }

// Notify writes one row per recipient: people only (user_… ids), once each, never the actor.
func (s Store) Notify(ctx context.Context, org string, to []string, e Event) error {
	seen := map[string]bool{e.Actor: true}
	var users []string
	for _, u := range to {
		if strings.HasPrefix(u, "user_") && !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
	}
	if len(users) == 0 {
		return nil
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO notifications (org_id, user_id, kind, text, actor, ticket_id, board_id)
		SELECT $1, u, $3, $4, $5, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid FROM unnest($2::text[]) AS u`,
		org, users, e.Kind, e.Text, e.Actor, e.TicketID, e.BoardID)
	return err
}

// BoardManagers are who approve and review on a board: the org's admins and the board team's leaders.
func (s Store) BoardManagers(ctx context.Context, org, board string) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT user_clerk_id FROM org_admins WHERE org_id = $1
		UNION SELECT tm.user_clerk_id FROM boards b JOIN team_members tm ON tm.team_id = b.team_id AND tm.leader
			WHERE b.id::text = $2 AND b.owner_clerk_id = $1`, org, board)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Person is someone who can be @mentioned, by any of their names (first name, full name, handle).
type Person struct {
	ID    string
	Names []string
}

var mentionEnd = regexp.MustCompile(`^[\p{L}\p{N}_]`)

// Mentioned returns who body @mentions, matching a name case-insensitively and whole ("@Ava" or
// "@Ava Smith", not "@Avalon").
// ponytail: scans every name per comment; fine for org-sized lists, index names if orgs get huge.
func Mentioned(body string, people []Person) []string {
	lower := strings.ToLower(body)
	var out []string
	for _, p := range people {
		for _, n := range p.Names {
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "" {
				continue
			}
			if hit(lower, "@"+n) {
				out = append(out, p.ID)
				break
			}
		}
	}
	return out
}

func hit(text, needle string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], needle)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(needle)
		// the @ starts a word (not an email address) and the name ends one ("@Al" isn't "@Alan")
		if (start == 0 || !mentionEnd.MatchString(lastRune(text[:start]))) && !mentionEnd.MatchString(text[end:]) {
			return true
		}
		i = end
	}
}

func lastRune(s string) string {
	r := []rune(s)
	return string(r[len(r)-1])
}

package audit

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TL-669: every org-level change is queued for the org log, with who did it, on a paid plan; nothing for
// a Free organization. Driven by the tables, as every handler (API, MCP, Clerk webhook, imports) is.
func TestOrgEventsQueued(t *testing.T) {
	ctx := context.Background()
	p := db(t)
	run := func(org string, paid bool) []queued {
		t.Helper()
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var project, board, team, sprint, key, invite, hook, imp string
		steps := []struct {
			sql  string
			args []any
			into *string
		}{
			{`SELECT set_config('tracklines.actor', 'user_admin', true)`, nil, nil},
			{`INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Shop') RETURNING id`, []any{org}, &project},
			{`UPDATE projects SET name = 'Store' WHERE owner_clerk_id = $1`, []any{org}, nil},
			{`INSERT INTO boards (project_id, owner_clerk_id, name) SELECT id, $1, 'Web' FROM projects WHERE owner_clerk_id = $1 RETURNING id`, []any{org}, &board},
			{`UPDATE boards SET name = 'Site' WHERE owner_clerk_id = $1`, []any{org}, nil},
			{`UPDATE boards SET archived_at = now() WHERE owner_clerk_id = $1`, []any{org}, nil},
			{`UPDATE boards SET archived_at = NULL WHERE owner_clerk_id = $1`, []any{org}, nil},
			{`INSERT INTO sprints (board_id, number, length_days, ends_at) SELECT id, 1, 14, now() + interval '14 days' FROM boards WHERE owner_clerk_id = $1 RETURNING id`, []any{org}, &sprint},
			{`UPDATE sprints SET closed_at = now() WHERE board_id IN (SELECT id FROM boards WHERE owner_clerk_id = $1)`, []any{org}, nil},
			{`INSERT INTO teams (org_id, name) VALUES ($1, 'Platform') RETURNING id`, []any{org}, &team},
			{`INSERT INTO team_members (team_id, user_clerk_id) SELECT id, 'user_sam' FROM teams WHERE org_id = $1`, []any{org}, nil},
			{`UPDATE team_members SET leader = true WHERE team_id IN (SELECT id FROM teams WHERE org_id = $1)`, []any{org}, nil},
			{`DELETE FROM team_members WHERE team_id IN (SELECT id FROM teams WHERE org_id = $1)`, []any{org}, nil},
			{`DELETE FROM teams WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO api_keys (owner_clerk_id, org_id, kind, name, prefix, hash) VALUES ('user_admin', $1, 'service', 'CI', 'tl_s_ab', decode(md5($1), 'hex')) RETURNING id`, []any{org}, &key},
			{`UPDATE api_keys SET revoked_at = now() WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO invitations (org_id, clerk_invitation_id, email_address, scope, invited_by, expires_at) VALUES ($1, $1 || '_inv', 'kim@example.com', 'organization', 'user_admin', now() + interval '7 days') RETURNING id`, []any{org}, &invite},
			{`UPDATE invitations SET status = 'revoked' WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'user_sam', 'user_admin')`, []any{org}, nil},
			{`DELETE FROM org_admins WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO webhook_endpoints (org_id, url, events, secret, secret_hint, created_by) VALUES ($1, 'https://Hooks.Example.com/t/SECRET-TOKEN?x=1', '{ticket.created}', '\x00', 'ab', 'user_admin') RETURNING id`, []any{org}, &hook},
			{`UPDATE webhook_endpoints SET events = '{ticket.created,ticket.moved}' WHERE org_id = $1`, []any{org}, nil},
			{`DELETE FROM webhook_endpoints WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO org_integrations (org_id, provider, secret, updated_by, account) VALUES ($1, 'bugfixes', '\x00', 'user_admin', 'acme')`, []any{org}, nil},
			{`DELETE FROM org_integrations WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO calendar_connections (org_id, domain, admin_email, connected_by) VALUES ($1, 'acme.com', 'it@acme.com', 'user_admin')`, []any{org}, nil},
			{`UPDATE calendar_connections SET domain = 'acme.io' WHERE org_id = $1`, []any{org}, nil},
			{`DELETE FROM calendar_connections WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO jira_connections (org_id, site, email, token, created_by, expires_at) VALUES ($1, 'acme.atlassian.net', 'it@acme.com', '\x00', 'user_admin', now() + interval '90 days')`, []any{org}, nil},
			{`DELETE FROM jira_connections WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO linear_connections (org_id, workspace, token, created_by, expires_at) VALUES ($1, 'Acme', '\x00', 'user_admin', now() + interval '90 days')`, []any{org}, nil},
			{`DELETE FROM linear_connections WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO chat_webhooks (org_id, url, kinds, updated_by) VALUES ($1, '\x00', '{done}', 'user_admin')`, []any{org}, nil},
			{`DELETE FROM chat_webhooks WHERE org_id = $1`, []any{org}, nil},
			{`INSERT INTO imports (org_id, source, plan, created_by, total) VALUES ($1, 'jira', '{}', 'user_admin', 12) RETURNING id`, []any{org}, &imp},
			{`UPDATE imports SET state = 'done', progress = 12 WHERE org_id = $1`, []any{org}, nil},
			{`DELETE FROM boards WHERE owner_clerk_id = $1`, []any{org}, nil},
			{`DELETE FROM projects WHERE owner_clerk_id = $1`, []any{org}, nil},
			{`INSERT INTO deleted_orgs (org_id) VALUES ($1)`, []any{org}, nil},
		}
		if paid {
			steps = append(steps[:1:1], append([]struct {
				sql  string
				args []any
				into *string
			}{{`INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ($1, 'active', 'pro')`, []any{org}, nil}}, steps[1:]...)...)
		}
		for _, s := range steps {
			if s.into != nil {
				err = tx.QueryRow(ctx, s.sql, s.args...).Scan(s.into)
			} else {
				_, err = tx.Exec(ctx, s.sql, s.args...)
			}
			if err != nil {
				t.Fatalf("%s: %v", s.sql, err)
			}
		}
		rows, err := tx.Query(ctx, `SELECT id::text, org_id, actor_kind, action, actor, target_id, data FROM audit_outbox
			WHERE org_id = $1 AND action <> 'org.plan_changed' ORDER BY queued`, org)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []queued
		for rows.Next() {
			var q queued
			if err := rows.Scan(&q.ID, &q.Org, &q.ActorKind, &q.Action, &q.Actor, &q.TargetID, &q.Data); err != nil {
				t.Fatal(err)
			}
			out = append(out, q)
		}
		return out
	}

	got := run("org_oe_"+uuid.NewString()[:8], true)
	var actions []string
	for _, q := range got {
		actions = append(actions, q.Action)
		if q.Actor == nil || *q.Actor != "user_admin" || q.ActorKind != "person" {
			t.Errorf("%s: by %v (%s)", q.Action, q.Actor, q.ActorKind)
		}
	}
	want := []string{
		"project.created", "project.renamed", "board.created", "board.renamed", "board.archived", "board.restored",
		"sprint.started", "sprint.closed", "team.created", "team.members_changed", "team.members_changed", "team.members_changed", "team.deleted",
		"api_key.created", "api_key.revoked", "member.invited", "member.invitation_revoked", "admin.granted", "admin.revoked",
		"webhook.created", "webhook.updated", "webhook.deleted",
		"integration.connected", "integration.disconnected", // bugfixes
		"integration.connected", "calendar.domain_changed", "integration.disconnected", // google calendar
		"integration.connected", "integration.disconnected", "integration.connected", "integration.disconnected", // jira, linear
		"integration.connected", "integration.disconnected", // google chat
		"import.run", "board.deleted", "project.deleted", "org.deleted",
	}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("queued:\n%v\nwant:\n%v", actions, want)
	}
	find := func(action string, n int) queued {
		for _, q := range got {
			if q.Action == action {
				if n == 0 {
					return q
				}
				n--
			}
		}
		t.Fatalf("no %s", action)
		return queued{}
	}
	all := fmt.Sprint(got)
	for _, secret := range []string{"SECRET-TOKEN", "kim@example.com", "it@acme.com", "hash"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q in the org log: %s", secret, all)
		}
	}
	for action, want := range map[string]string{
		"project.renamed":         "map[from:Shop to:Store]",
		"board.archived":          "map[name:Site]",
		"sprint.closed":           "map[board:",
		"api_key.created":         "kind:service name:CI owner:user_admin read_only:false",
		"webhook.updated":         "map[enabled:true events:[ticket.created ticket.moved] host:hooks.example.com]",
		"calendar.domain_changed": "map[from:acme.com to:acme.io]",
		"import.run":              "map[created:12 source:jira state:done total:12]",
		"member.invited":          "map[scope:organization target:<nil>]",
	} {
		if d := fmt.Sprint(find(action, 0).Data); !strings.Contains(d, want) {
			t.Errorf("%s: %s, want %s", action, d, want)
		}
	}
	if d := fmt.Sprint(find("team.members_changed", 1).Data); !strings.Contains(d, "change:made_leader") || !strings.Contains(d, "user:user_sam") {
		t.Errorf("leader: %s", d)
	}
	if d := fmt.Sprint(find("integration.connected", 1).Data); d != "map[domain:acme.com provider:google-calendar]" {
		t.Errorf("calendar: %s", d)
	}

	if free := run("org_of_"+uuid.NewString()[:8], false); len(free) != 0 {
		t.Fatalf("a Free org's changes were queued: %v", free)
	}
}

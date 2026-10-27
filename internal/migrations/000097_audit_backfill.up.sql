-- Ticket history written before the audit outbox (TL-667), queued for the auditor under the same ids the
-- trigger gives newer rows (audit_stage_id), so the relay delivers it and nothing is stored twice: a row
-- already sent is accepted again and changes nothing. History reads merge the auditor with what's still
-- queued here, so it's complete while this is being delivered.
INSERT INTO audit_outbox (id, org_id, at, actor, actor_kind, action, target_type, target_id, project_id, data)
SELECT audit_stage_id(h.id), p.owner_clerk_id, h.at, h.actor, audit_actor_kind(h.actor), 'ticket.stage',
       'ticket', h.ticket_id::text, h.project_id,
       jsonb_strip_nulls(jsonb_build_object('stage', h.stage, 'outcome', h.outcome, 'stage_order', h.stage_order,
           'board_id', h.board_id, 'board_name', h.board_name, 'column_id', h.column_id,
           'column_name', h.column_name, 'resolution', h.resolution)) || jsonb_build_object('done', h.done)
FROM ticket_history h JOIN projects p ON p.id = h.project_id
ORDER BY h.id
ON CONFLICT (id) DO NOTHING;

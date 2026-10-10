-- Auto-completion of parents (2026-10-09) only reacts to a sub-ticket finishing, so parents whose
-- sub-tickets had all finished before then stayed open. Complete them the way the app does: a
-- backlog parent is resolved, a board parent moves to its board's Done column. Repeats until
-- nothing changes, so it carries up chains of parents.
DO $$
DECLARE changed integer;
BEGIN
  LOOP
    WITH finished AS (
      SELECT p.id FROM tickets p
      WHERE p.resolved_at IS NULL
        AND (p.column_id IS NULL OR p.column_id <> (SELECT dc.id FROM columns dc WHERE dc.board_id = p.board_id ORDER BY dc.position DESC LIMIT 1))
        AND EXISTS (SELECT 1 FROM tickets c WHERE c.parent_id = p.id)
        AND NOT EXISTS (SELECT 1 FROM tickets c WHERE c.parent_id = p.id AND NOT (
          c.resolved_at IS NOT NULL OR (c.column_id IS NOT NULL AND c.column_id = (
            SELECT dc.id FROM columns dc WHERE dc.board_id = c.board_id ORDER BY dc.position DESC LIMIT 1))))
    )
    UPDATE tickets p SET
      resolved_at = CASE WHEN p.board_id IS NULL THEN now() END,
      column_id = CASE WHEN p.board_id IS NOT NULL
        THEN (SELECT dc.id FROM columns dc WHERE dc.board_id = p.board_id ORDER BY dc.position DESC LIMIT 1) END,
      done_at = COALESCE(p.done_at, now()),
      updated_at = now()
    FROM finished f WHERE p.id = f.id;
    GET DIAGNOSTICS changed = ROW_COUNT;
    EXIT WHEN changed = 0;
  END LOOP;
END $$;

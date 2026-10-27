-- The working week (TL-510, TL-702): an organization's, and a project's own where it differs. No row: Mon–Fri,
-- Monday first, 09:00–17:00, as it was hard-coded. Days are 0 (Sunday) to 6 (Saturday); hours are minutes
-- from midnight, in each person's own timezone (owner, 27 Oct).
CREATE TABLE working_weeks (
    org_id       text NOT NULL,
    project_id   uuid REFERENCES projects (id) ON DELETE CASCADE, -- null: the organization's
    days         smallint[] NOT NULL CHECK (cardinality(days) BETWEEN 1 AND 7 AND days <@ '{0,1,2,3,4,5,6}'),
    week_start   smallint NOT NULL CHECK (week_start BETWEEN 0 AND 6),
    start_minute integer NOT NULL CHECK (start_minute BETWEEN 0 AND 1380),
    end_minute   integer NOT NULL CHECK (end_minute <= 1440 AND end_minute - start_minute >= 60),
    updated_by   text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (org_id, project_id)
);

-- working_days: a board's working days (its project's week, else its organization's, else Mon–Fri).
CREATE FUNCTION working_days(board uuid) RETURNS smallint[] LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT w.days FROM working_weeks w JOIN boards b ON w.project_id = b.project_id WHERE b.id = board),
        (SELECT w.days FROM working_weeks w JOIN boards b ON w.org_id = b.owner_clerk_id AND w.project_id IS NULL WHERE b.id = board),
        '{1,2,3,4,5}')
$$;

-- working_end: when a sprint of board ending at ends really ends: on a working day (UTC), running on to
-- the next one when its length lands on a day off (owner, 27 Oct: sprint dates follow the working week).
CREATE FUNCTION working_end(board uuid, ends timestamptz) RETURNS timestamptz LANGUAGE plpgsql STABLE AS $$
DECLARE
    days smallint[] := working_days(board);
BEGIN
    FOR i IN 1..7 LOOP
        EXIT WHEN extract(dow FROM ends AT TIME ZONE 'UTC')::smallint = ANY (days);
        ends := ends + interval '1 day';
    END LOOP;
    RETURN ends;
END $$;

-- the org log: who changed the working week, and to what (paid plans, like the rest of it)
CREATE FUNCTION queue_working_week() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r working_weeks := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
BEGIN
    PERFORM audit_org(r.org_id, 'working_week.changed', CASE WHEN r.project_id IS NULL THEN 'org' ELSE 'project' END,
        COALESCE(r.project_id::text, r.org_id), r.project_id,
        CASE WHEN TG_OP = 'DELETE' THEN jsonb_build_object('reset', true)
        ELSE jsonb_build_object('days', to_jsonb(r.days), 'week_start', r.week_start, 'start_minute', r.start_minute, 'end_minute', r.end_minute) END,
        r.updated_by);
    RETURN NULL;
END $$;
CREATE TRIGGER queue_working_week AFTER INSERT OR UPDATE OR DELETE ON working_weeks FOR EACH ROW EXECUTE FUNCTION queue_working_week();

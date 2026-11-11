-- TL-789: each connected organization verifies its own flags.gg deliveries.
ALTER TABLE org_integrations ADD COLUMN flags_webhook_secret bytea;

CREATE TABLE ticket_flag_states (
    ticket_id uuid NOT NULL REFERENCES tickets_all (id) ON DELETE CASCADE,
    agent_id text NOT NULL,
    environment_id text NOT NULL,
    flag_id text NOT NULL,
    name text NOT NULL,
    enabled boolean NOT NULL,
    rollout integer NOT NULL CHECK (rollout BETWEEN 0 AND 100),
    archived boolean NOT NULL,
    event_at timestamptz NOT NULL,
    PRIMARY KEY (ticket_id, agent_id, environment_id)
);

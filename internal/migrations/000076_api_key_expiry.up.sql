-- API keys expire (TL-433): 90 days unless the creator picks otherwise (up to 366). The default also
-- gives every key that predates this 90 days from now, so nothing stops working at deploy.
ALTER TABLE api_keys ADD COLUMN expires_at timestamptz NOT NULL DEFAULT now() + interval '90 days';

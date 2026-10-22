-- Embeddable roadmaps & request forms (TL-494/495): the origins an organisation allows its public
-- widgets to be loaded on. Dynamic CORS reflects Access-Control-Allow-Origin only for a listed origin.
CREATE TABLE embed_origins (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    org        text NOT NULL,                 -- the owning organisation (Clerk org id)
    origin     text NOT NULL,                 -- scheme://host[:port], lowercased, no path/trailing slash
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org, origin)
);
CREATE INDEX embed_origins_org_idx ON embed_origins (org);

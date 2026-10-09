-- Each board picks how its tickets are estimated; values are checked in the app (boards.Scales).
ALTER TABLE boards ADD COLUMN estimate_scale TEXT NOT NULL DEFAULT 'none'
    CHECK (estimate_scale IN ('none', 'fibonacci', 'tshirt', 'powers', 'linear'));
ALTER TABLE tickets ADD COLUMN estimate TEXT;

CREATE TABLE IF NOT EXISTS system_init (
    singleton      boolean PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    initialized    boolean NOT NULL DEFAULT FALSE,
    init_mode      text NOT NULL DEFAULT '',
    initialized_at timestamptz,
    initialized_by text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

INSERT INTO system_init (singleton, initialized, init_mode, initialized_by)
VALUES (TRUE, FALSE, '', '')
ON CONFLICT (singleton) DO NOTHING;

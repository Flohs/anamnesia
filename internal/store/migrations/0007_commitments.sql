-- +goose Up
CREATE TABLE commitments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id),
    project_id  uuid REFERENCES projects(id),
    owner       text NOT NULL,
    beneficiary text NOT NULL,
    body        text NOT NULL,
    due_at      timestamptz,
    status      text NOT NULL DEFAULT 'open'
                CHECK (status IN ('open','done','dropped')),
    source_id   uuid REFERENCES sources(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX commitments_user_status_due
    ON commitments (user_id, status, due_at);
CREATE INDEX commitments_project_status
    ON commitments (project_id, status)
    WHERE project_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS commitments;

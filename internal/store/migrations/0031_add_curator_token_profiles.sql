-- +goose Up
-- Existing credentials retain their full authority. Curator profiles are
-- available only to agent tokens so a bootstrap or operator credential can
-- never be narrowed or mislabeled as a background curator by row mutation.
ALTER TABLE tokens
    ADD COLUMN access_profile TEXT NOT NULL DEFAULT 'full';

ALTER TABLE tokens
    ADD CONSTRAINT tokens_access_profile_kind_check CHECK (
        (kind = 'agent' AND access_profile IN ('full', 'curator-preview', 'curator-apply'))
        OR
        (kind <> 'agent' AND access_profile = 'full')
    );

-- +goose Down
ALTER TABLE tokens DROP CONSTRAINT tokens_access_profile_kind_check;
ALTER TABLE tokens DROP COLUMN access_profile;

-- +goose Up
-- Plan snapshot on the account: which plan the account is on, the resolved
-- limits it is held to, and the features it includes (issue #31).
--
-- The cell stores the SNAPSHOT the control plane computed — never the plan
-- catalog itself. The control plane owns the effective-plan function
-- (override ?? entitled ?? free) and pushes its result here via the
-- provision-token-gated :plan endpoint; the cell then enforces autonomously,
-- with no catalog, no network, and no billing knowledge. Comped accounts and
-- enterprise custom terms work the day the control plane supports them,
-- because the cell only ever sees resolved numbers.
--
-- plan_limits semantics: a key caps the LIVE count of that resource
-- account-wide ('realms', 'agents'); a MISSING key means unlimited. The
-- default '{}' therefore means "no caps", which is deliberate: self-hosted
-- deployments have no control plane and are never plan-capped, and a Cloud
-- cell enforces nothing until its control plane has explicitly applied a
-- snapshot. Enforcement style per the decided design: creation is
-- HARD-capped (a clear refusal at create time), data is never silently
-- dropped.
--
-- plan_features mirrors the plan's feature list ('memory', 'facts',
-- 'secrets', 'collaboration', 'support', ...). Stored now for the
-- capabilities surface; feature GATING wires in as those subsystems land.
-- plan itself is a display label ('free' matches the public catalog's
-- zero-value plan); enforcement reads only plan_limits/plan_features.
ALTER TABLE accounts ADD COLUMN plan TEXT NOT NULL DEFAULT 'free';
ALTER TABLE accounts ADD COLUMN plan_limits JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE accounts ADD COLUMN plan_features JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose Down
ALTER TABLE accounts DROP COLUMN plan_features;
ALTER TABLE accounts DROP COLUMN plan_limits;
ALTER TABLE accounts DROP COLUMN plan;

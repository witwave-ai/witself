-- +goose Up
-- Account placement policy is the owner's ranked/pinned preference set for
-- future cell placement. The control plane will use this during archived
-- restore and later live rebalance; the cell stores the account-owned source of
-- truth so exports carry it with the rest of the account.
ALTER TABLE accounts ADD COLUMN placement_policy JSONB NOT NULL DEFAULT '{
  "preferred_clouds": [],
  "preferred_regions": ["usw2", "use1"],
  "preferred_channels": ["stable", "edge", "experimental"],
  "allowed_clouds": [],
  "allowed_regions": [],
  "allowed_channels": [],
  "rebalance_on": ["cloud", "channel"]
}'::jsonb;

-- +goose Down
ALTER TABLE accounts DROP COLUMN placement_policy;

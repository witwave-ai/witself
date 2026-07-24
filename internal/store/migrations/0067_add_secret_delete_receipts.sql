-- +goose Up
-- Widen the sealed-plane receipt vocabulary without exposing request payloads
-- or weakening the target-kind invariant. Build and validate replacement
-- constraints before the short metadata-only swap.
ALTER TABLE secret_mutation_receipts
  ADD CONSTRAINT secret_mutation_receipts_operation_delete_check
  CHECK (operation IN
    ('key_register', 'secret_create', 'secret_update', 'secret_archive',
     'secret_restore', 'secret_delete', 'dek_rewrap', 'field_access')) NOT VALID;

ALTER TABLE secret_mutation_receipts
  ADD CONSTRAINT secret_mutation_receipts_target_delete_check
  CHECK (
    (operation = 'key_register' AND target_kind = 'key_epoch' AND target_id ~ '^avk_') OR
    (operation IN ('secret_create', 'secret_update', 'secret_archive', 'secret_restore', 'secret_delete') AND
     target_kind = 'secret' AND target_id ~ '^sec_') OR
    (operation = 'dek_rewrap' AND target_kind = 'dek' AND target_id ~ '^dek_') OR
    (operation = 'field_access' AND target_kind = 'field' AND target_id ~ '^fld_')
  ) NOT VALID;

ALTER TABLE secret_mutation_receipts
  VALIDATE CONSTRAINT secret_mutation_receipts_operation_delete_check;
ALTER TABLE secret_mutation_receipts
  VALIDATE CONSTRAINT secret_mutation_receipts_target_delete_check;

ALTER TABLE secret_mutation_receipts
  DROP CONSTRAINT secret_mutation_receipts_operation_check;
ALTER TABLE secret_mutation_receipts
  DROP CONSTRAINT secret_mutation_receipts_check;
ALTER TABLE secret_mutation_receipts
  RENAME CONSTRAINT secret_mutation_receipts_operation_delete_check
  TO secret_mutation_receipts_operation_check;
ALTER TABLE secret_mutation_receipts
  RENAME CONSTRAINT secret_mutation_receipts_target_delete_check
  TO secret_mutation_receipts_check;

-- +goose Down
-- A delete receipt is durable retry/audit evidence and cannot be represented
-- by the legacy schema. Refuse downgrade before changing constraints.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM secret_mutation_receipts WHERE operation = 'secret_delete'
  ) THEN
    RAISE EXCEPTION 'cannot restore legacy secret receipt constraints while delete receipts exist';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE secret_mutation_receipts
  ADD CONSTRAINT secret_mutation_receipts_operation_legacy_check
  CHECK (operation IN
    ('key_register', 'secret_create', 'secret_update', 'secret_archive',
     'secret_restore', 'dek_rewrap', 'field_access')) NOT VALID;
ALTER TABLE secret_mutation_receipts
  ADD CONSTRAINT secret_mutation_receipts_target_legacy_check
  CHECK (
    (operation = 'key_register' AND target_kind = 'key_epoch' AND target_id ~ '^avk_') OR
    (operation IN ('secret_create', 'secret_update', 'secret_archive', 'secret_restore') AND
     target_kind = 'secret' AND target_id ~ '^sec_') OR
    (operation = 'dek_rewrap' AND target_kind = 'dek' AND target_id ~ '^dek_') OR
    (operation = 'field_access' AND target_kind = 'field' AND target_id ~ '^fld_')
  ) NOT VALID;
ALTER TABLE secret_mutation_receipts
  VALIDATE CONSTRAINT secret_mutation_receipts_operation_legacy_check;
ALTER TABLE secret_mutation_receipts
  VALIDATE CONSTRAINT secret_mutation_receipts_target_legacy_check;
ALTER TABLE secret_mutation_receipts
  DROP CONSTRAINT secret_mutation_receipts_operation_check;
ALTER TABLE secret_mutation_receipts
  DROP CONSTRAINT secret_mutation_receipts_check;
ALTER TABLE secret_mutation_receipts
  RENAME CONSTRAINT secret_mutation_receipts_operation_legacy_check
  TO secret_mutation_receipts_operation_check;
ALTER TABLE secret_mutation_receipts
  RENAME CONSTRAINT secret_mutation_receipts_target_legacy_check
  TO secret_mutation_receipts_check;

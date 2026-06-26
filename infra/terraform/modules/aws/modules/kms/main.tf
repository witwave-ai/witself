# AWS KMS submodule — sealed-plane CMK and the envelope-operations IAM policy.
#
# SCAFFOLD: commented placeholders. This submodule is instantiated by the AWS
# module only when sealed_plane_enabled is true. The open plane never reaches
# this code. See docs/key-hierarchy.md and docs/encryption-model.md.

locals {
  provision_new_key = var.existing_kms_key_arn == ""

  # Envelope operations the witself-server deployment identity needs against the
  # CMK — and nothing more. Deliberately excludes ScheduleKeyDeletion and key-
  # policy administration: key administration stays with operators.
  envelope_actions = [
    "kms:Encrypt",
    "kms:Decrypt",
    "kms:GenerateDataKey",
    "kms:GenerateDataKeyWithoutPlaintext",
    "kms:DescribeKey",
  ]
}

###############################################################################
# CMK — provisioned only when no existing key ARN is supplied (BYOK skips this).
#
# The CMK is the root of the envelope: it wraps per-realm KEKs, which wrap per-
# secret/field DEKs. Terraform never sees a KEK or DEK. Rotation is enabled by
# default. Losing this key crypto-shreds all sealed-plane values it roots and is
# unrecoverable; it does not affect the open plane.
###############################################################################

# resource "aws_kms_key" "sealed_plane" {
#   count                   = local.provision_new_key ? 1 : 0
#   description             = "${var.name} sealed-plane CMK (secrets + TOTP envelope root)"
#   enable_key_rotation     = var.key_rotation_enabled
#   deletion_window_in_days = 30
#   # Key policy scoped to the deployment identity for envelope ops, plus an
#   # operator/admin principal for key administration. Not account-root-open.
#   tags = var.tags
# }
#
# resource "aws_kms_alias" "sealed_plane" {
#   count         = local.provision_new_key ? 1 : 0
#   name          = "alias/${var.name}-sealed-plane"
#   target_key_id = aws_kms_key.sealed_plane[0].key_id
# }

###############################################################################
# IAM policy — envelope operations on the CMK, attached to the IRSA role.
#
# Scoped to local.envelope_actions on the single key ARN. The deployment identity
# must not hold key administration or deletion permissions.
###############################################################################

# data "aws_iam_policy_document" "envelope" {
#   statement {
#     sid       = "WitselfSealedPlaneEnvelopeOps"
#     effect    = "Allow"
#     actions   = local.envelope_actions
#     resources = [local.provision_new_key ? aws_kms_key.sealed_plane[0].arn : var.existing_kms_key_arn]
#   }
# }
#
# resource "aws_iam_role_policy" "envelope" {
#   count  = var.deployment_role_arn != "" ? 1 : 0
#   name   = "${var.name}-sealed-plane-kms"
#   role   = var.deployment_role_arn
#   policy = data.aws_iam_policy_document.envelope.json
# }

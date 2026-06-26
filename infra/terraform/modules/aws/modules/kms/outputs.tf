# AWS KMS submodule — outputs.
#
# Surfaces the KMS provider and key ARN for the parent module, which forwards them
# to Helm (WITSELF_KMS_PROVIDER / WITSELF_KMS_KEY_ID). The ARN is a key
# identifier, never key material.

output "provider" {
  description = "KMS provider identifier for the sealed plane."
  value       = "aws-kms"
}

output "key_arn" {
  description = "ARN of the sealed-plane CMK (provisioned or BYOK). A key identifier, never key material."
  # value = local.provision_new_key ? aws_kms_key.sealed_plane[0].arn : var.existing_kms_key_arn
  value = var.existing_kms_key_arn
}

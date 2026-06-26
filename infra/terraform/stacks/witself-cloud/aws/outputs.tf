# Witself Cloud AWS stack — outputs.
#
# Forwards the AWS module's Helm-handoff references for the managed deployment.
# Only names, keys, and IDs — never raw credentials. Sensitive production values
# are resolved through private overlays, not exposed here.

output "namespace" {
  description = "Kubernetes namespace for witself-server."
  value       = module.witself.namespace
}

output "service_account_annotations" {
  description = "IRSA annotations for the witself-server ServiceAccount."
  value       = module.witself.service_account_annotations
}

output "database_secret_name" {
  description = "Database connection Secret name (Helm: database.existingSecret.name)."
  value       = module.witself.database_secret_name
}

output "blob_bucket" {
  description = "S3 bucket for exports, attachments, bundles, and backups."
  value       = module.witself.blob_bucket
}

output "public_url" {
  description = "Public URL / ingress host for managed Witself Cloud."
  value       = module.witself.public_url
}

output "sealed_plane_enabled" {
  description = "Whether the sealed plane is enabled (always true for managed Cloud)."
  value       = module.witself.sealed_plane_enabled
}

output "kms_provider" {
  description = "Sealed-plane KMS provider (Helm: kms.provider -> WITSELF_KMS_PROVIDER)."
  value       = module.witself.kms_provider
}

output "kms_key_id" {
  description = "Sealed-plane CMK ARN (Helm: kms.keyRef -> WITSELF_KMS_KEY_ID). A key identifier, never key material."
  value       = module.witself.kms_key_id
}

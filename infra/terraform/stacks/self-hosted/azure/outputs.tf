# Self-hosted Azure stack — outputs.
#
# SKELETON: AWS-first. Forwards the Azure module's Helm-handoff references.

output "namespace" {
  description = "Kubernetes namespace for witself-server."
  value       = module.witself.namespace
}

output "service_account_annotations" {
  description = "Azure Workload Identity annotations for the witself-server ServiceAccount."
  value       = module.witself.service_account_annotations
}

output "database_secret_name" {
  description = "Database connection Secret name (Helm: database.existingSecret.name)."
  value       = module.witself.database_secret_name
}

output "blob_container" {
  description = "Azure Blob Storage account/container for exports, attachments, bundles, and backups."
  value       = module.witself.blob_container
}

output "sealed_plane_enabled" {
  description = "Whether the sealed plane (secrets + TOTP) is enabled."
  value       = module.witself.sealed_plane_enabled
}

output "kms_provider" {
  description = "Sealed-plane KMS provider (Helm: kms.provider). Null when disabled."
  value       = module.witself.kms_provider
}

output "kms_key_id" {
  description = "Sealed-plane Azure Key Vault key reference (Helm: kms.keyRef). A key identifier, never key material. Null when disabled."
  value       = module.witself.kms_key_id
}

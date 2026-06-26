# Self-hosted AWS stack — outputs.
#
# Forwards the AWS module's Helm-handoff references. Feed these into the witself
# Helm chart values (database.existingSecret, serviceAccount.annotations, kms.*,
# etc.). Terraform passes only names, keys, and IDs — never raw credentials.

output "namespace" {
  description = "Kubernetes namespace for witself-server."
  value       = module.witself.namespace
}

output "service_account_name" {
  description = "witself-server ServiceAccount name."
  value       = module.witself.service_account_name
}

output "service_account_annotations" {
  description = "IRSA annotations for the witself-server ServiceAccount (Helm: serviceAccount.annotations)."
  value       = module.witself.service_account_annotations
}

output "database_secret_name" {
  description = "Database connection Secret name (Helm: database.existingSecret.name)."
  value       = module.witself.database_secret_name
}

output "database_secret_url_key" {
  description = "Database connection Secret key (Helm: database.existingSecret.urlKey)."
  value       = module.witself.database_secret_url_key
}

output "pgvector_enabled" {
  description = "Whether Postgres has pgvector enabled (hard gate for the open plane)."
  value       = module.witself.pgvector_enabled
}

output "blob_bucket" {
  description = "S3 bucket for exports, attachments, bundles, and backups."
  value       = module.witself.blob_bucket
}

output "public_url" {
  description = "Public URL / ingress host for the deployment."
  value       = module.witself.public_url
}

output "sealed_plane_enabled" {
  description = "Whether the sealed plane (secrets + TOTP) is enabled."
  value       = module.witself.sealed_plane_enabled
}

output "kms_provider" {
  description = "Sealed-plane KMS provider (Helm: kms.provider -> WITSELF_KMS_PROVIDER). Null when disabled."
  value       = module.witself.kms_provider
}

output "kms_key_id" {
  description = "Sealed-plane CMK ARN (Helm: kms.keyRef -> WITSELF_KMS_KEY_ID). A key identifier, never key material. Null when disabled."
  value       = module.witself.kms_key_id
}

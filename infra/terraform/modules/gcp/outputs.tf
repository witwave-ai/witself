# GCP module — outputs consumed by the Helm chart.
#
# SKELETON: AWS-first. Mirrors the AWS module output surface so the Helm handoff
# is provider-consistent. Values are static/null placeholders until the resources
# in main.tf land. See docs/helm-chart.md "Terraform Handoff".

output "namespace" {
  description = "Kubernetes namespace for the witself-server deployment."
  value       = var.kubernetes_namespace
}

output "service_account_name" {
  description = "Name of the witself-server ServiceAccount."
  value       = var.service_account_name
}

output "service_account_annotations" {
  description = "Annotations for GCP Workload Identity (Helm: serviceAccount.annotations)."
  value = {
    # "iam.gke.io/gcp-service-account" = google_service_account.witself_server.email
  }
}

output "database_secret_name" {
  description = "Name of the Kubernetes Secret holding the database connection (Helm: database.existingSecret.name)."
  value       = "${var.name}-database"
}

output "database_secret_url_key" {
  description = "Key within the database Secret holding the connection URL (Helm: database.existingSecret.urlKey)."
  value       = "database-url"
}

output "pgvector_enabled" {
  description = "Whether the provisioned Postgres enables pgvector. A hard gate for the open plane."
  value       = true
}

output "embedding_vector_dimensions" {
  description = "Expected embedding vector dimensionality for the capabilities contract."
  value       = var.embedding_vector_dimensions
}

output "blob_bucket" {
  description = "Cloud Storage bucket for exports, attachments, bundles, and backups."
  value       = "${var.name}-blob"
}

output "sealed_plane_enabled" {
  description = "Whether the sealed plane (secrets + TOTP) is enabled."
  value       = var.sealed_plane_enabled
}

output "kms_provider" {
  description = "KMS provider for the sealed plane (Helm: kms.provider). Null when disabled."
  value       = var.sealed_plane_enabled ? "gcp-kms" : null
}

output "kms_key_id" {
  description = "Cloud KMS crypto key resource name for the sealed plane (Helm: kms.keyRef). A key identifier, never key material. Null when disabled."
  # value = var.sealed_plane_enabled ? google_kms_crypto_key.sealed_plane[0].id : null
  value = null
}

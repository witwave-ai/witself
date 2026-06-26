# Azure module — outputs consumed by the Helm chart.
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
  description = "Annotations for Azure Workload Identity (Helm: serviceAccount.annotations)."
  value = {
    # "azure.workload.identity/client-id" = azurerm_user_assigned_identity.witself_server.client_id
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

output "blob_container" {
  description = "Azure Blob Storage account/container for exports, attachments, bundles, and backups."
  value       = replace("${var.name}blob", "-", "")
}

output "sealed_plane_enabled" {
  description = "Whether the sealed plane (secrets + TOTP) is enabled."
  value       = var.sealed_plane_enabled
}

output "kms_provider" {
  description = "KMS provider for the sealed plane (Helm: kms.provider). Null when disabled."
  value       = var.sealed_plane_enabled ? "azure-key-vault" : null
}

output "kms_key_id" {
  description = "Azure Key Vault key reference for the sealed plane (Helm: kms.keyRef). A key identifier, never key material. Null when disabled."
  # value = var.sealed_plane_enabled ? azurerm_key_vault_key.sealed_plane_cmk[0].id : null
  value = null
}

# AWS module — outputs consumed by the Helm chart.
#
# These are the references the chart needs to deploy witself-server onto the
# provisioned substrate. Terraform passes only names, keys, IDs, and non-sensitive
# config — never raw credentials. See docs/helm-chart.md "Terraform Handoff".
#
# SCAFFOLD: values reference the commented placeholder resources in main.tf and
# are returned as null/static until those resources land. The output surface is
# the frozen contract.

output "namespace" {
  description = "Kubernetes namespace for the witself-server deployment."
  value       = var.kubernetes_namespace
}

output "service_account_name" {
  description = "Name of the witself-server ServiceAccount the IRSA role is bound to."
  value       = var.service_account_name
}

output "service_account_annotations" {
  description = "Annotations to apply to the witself-server ServiceAccount for IRSA workload identity (Helm: serviceAccount.annotations)."
  value = {
    # "eks.amazonaws.com/role-arn" = aws_iam_role.irsa.arn
  }
}

output "cluster_name" {
  description = "Name of the EKS cluster witself-server runs on."
  value       = var.create_cluster ? "${var.name}-eks" : var.existing_cluster_name
}

###############################################################################
# Database (Postgres with pgvector) — open plane system of record
###############################################################################

output "database_secret_name" {
  description = "Name of the Kubernetes Secret holding the database connection (Helm: database.existingSecret.name). Created via a deployment-native mechanism, never rendered with a raw URL."
  value       = "${var.name}-database"
}

output "database_secret_url_key" {
  description = "Key within the database Secret holding the connection URL (Helm: database.existingSecret.urlKey)."
  value       = "database-url"
}

output "pgvector_enabled" {
  description = "Whether the provisioned Postgres engine ships and enables pgvector. pgvector is a hard gate for the open plane."
  value       = true
}

output "embedding_vector_dimensions" {
  description = "Expected embedding vector dimensionality, for the capabilities contract to confirm semantic recall is available."
  value       = var.embedding_vector_dimensions
}

###############################################################################
# Object/blob storage
###############################################################################

output "blob_bucket" {
  description = "S3 bucket name for exports, attachments, diagnostic bundles, and backups. Null when blob storage is not provisioned."
  value       = var.create_blob_storage ? "${var.name}-blob" : null
}

###############################################################################
# Public URL
###############################################################################

output "public_url" {
  description = "Public URL / ingress host for the deployment. Null when no public hostname is configured."
  value       = var.public_hostname != "" ? "https://${var.public_hostname}" : null
}

###############################################################################
# Sealed plane — KMS (only meaningful when sealed_plane_enabled is true)
#
# Helm maps these to WITSELF_KMS_PROVIDER and WITSELF_KMS_KEY_ID on
# witself-server. kms_key_id is a key identifier (ARN), never key material.
###############################################################################

output "sealed_plane_enabled" {
  description = "Whether the sealed plane (secrets + TOTP) is enabled for this deployment."
  value       = var.sealed_plane_enabled
}

output "kms_provider" {
  description = "KMS provider for the sealed plane (Helm: kms.provider). Null when the sealed plane is disabled."
  value       = var.sealed_plane_enabled ? "aws-kms" : null
}

output "kms_key_id" {
  description = "KMS CMK ARN for the sealed plane (Helm: kms.keyRef -> WITSELF_KMS_KEY_ID). A key identifier, never key material. Null when the sealed plane is disabled."
  value       = var.sealed_plane_enabled ? one(module.kms[*].key_arn) : null
}

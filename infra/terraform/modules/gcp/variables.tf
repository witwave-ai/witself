# GCP module — input variables.
#
# SKELETON: AWS-first. GCP is a planned target (GKE, Cloud SQL Postgres with
# pgvector, Cloud Storage, Workload Identity, Cloud KMS for the sealed plane).
# See docs/terraform-infrastructure.md "GCP Target".

variable "name" {
  description = "Name prefix for all provisioned resources."
  type        = string
}

variable "project_id" {
  description = "GCP project ID to deploy into."
  type        = string
}

variable "region" {
  description = "GCP region."
  type        = string
  default     = "us-central1"
}

variable "kubernetes_namespace" {
  description = "Kubernetes namespace for the witself-server deployment."
  type        = string
  default     = "witself"
}

variable "service_account_name" {
  description = "Name of the witself-server Kubernetes ServiceAccount."
  type        = string
  default     = "witself-server"
}

variable "embedding_vector_dimensions" {
  description = "Expected embedding vector dimensionality, surfaced as an output for the capabilities contract."
  type        = number
  default     = 1024
}

variable "sealed_plane_enabled" {
  description = "Enable the sealed plane (secrets + TOTP). When true, a Cloud KMS key is provisioned for the envelope root. The open plane never needs KMS."
  type        = bool
  default     = false
}

variable "labels" {
  description = "Labels applied to resources that support them."
  type        = map(string)
  default     = {}
}

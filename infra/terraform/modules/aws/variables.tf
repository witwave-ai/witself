# AWS module — input variables.
#
# Scaffold note: these inputs frame the substrate the AWS module provisions
# (EKS, RDS/Aurora Postgres with pgvector, S3, IRSA workload identity, sealed-
# plane KMS, networking). Exact variable set will evolve during implementation;
# the boundaries are what matter here. See docs/terraform-infrastructure.md.

###############################################################################
# General
###############################################################################

variable "name" {
  description = "Name prefix for all provisioned resources (e.g. \"witself-prod\")."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}[a-z0-9]$", var.name))
    error_message = "name must be lowercase alphanumeric with hyphens, 3-32 chars, starting with a letter."
  }
}

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "us-east-1"
}

variable "tags" {
  description = "Tags applied to all resources that support tagging."
  type        = map(string)
  default     = {}
}

###############################################################################
# Networking
#
# Sized to also carry inter-agent messaging if a post-v0 transport ever needs
# cross-pod or cross-AZ delivery paths. v0 messaging is Postgres-backed and
# needs nothing beyond the cluster + database. See docs/inter-agent-messaging.md.
###############################################################################

variable "vpc_id" {
  description = "Existing VPC ID to deploy into. Leave empty to provision a new VPC (placeholder)."
  type        = string
  default     = ""
}

variable "vpc_cidr" {
  description = "CIDR block for a newly provisioned VPC (ignored when vpc_id is set)."
  type        = string
  default     = "10.0.0.0/16"
}

variable "private_subnet_ids" {
  description = "Existing private subnet IDs for the cluster and database. Empty provisions new subnets (placeholder)."
  type        = list(string)
  default     = []
}

###############################################################################
# Kubernetes (EKS)
###############################################################################

variable "kubernetes_version" {
  description = "EKS control plane version."
  type        = string
  default     = "1.30"
}

variable "create_cluster" {
  description = "Provision a new EKS cluster. When false, the module integrates with an existing cluster."
  type        = bool
  default     = true
}

variable "existing_cluster_name" {
  description = "Name of an existing EKS cluster to integrate with (used when create_cluster is false)."
  type        = string
  default     = ""
}

###############################################################################
# PostgreSQL (RDS/Aurora) with pgvector
#
# pgvector is a hard prerequisite of the backend, not an afterthought. The engine
# version must ship pgvector; "vector" is added to shared_preload_libraries via a
# managed parameter group; CREATE EXTENSION IF NOT EXISTS vector runs as a
# bootstrap step. Its absence is a deployment error, not a recall-degrade
# trigger. See docs/storage.md.
###############################################################################

variable "postgres_engine" {
  description = "Postgres engine: \"aurora-postgresql\" or \"postgres\" (RDS)."
  type        = string
  default     = "aurora-postgresql"

  validation {
    condition     = contains(["aurora-postgresql", "postgres"], var.postgres_engine)
    error_message = "postgres_engine must be \"aurora-postgresql\" or \"postgres\"."
  }
}

variable "postgres_engine_version" {
  description = "Postgres engine version. Must be a version that ships the pgvector extension."
  type        = string
  default     = "16.4"
}

variable "postgres_instance_class" {
  description = "Instance class for the database."
  type        = string
  default     = "db.r6g.large"
}

variable "database_name" {
  description = "Initial database name created for witself-server."
  type        = string
  default     = "witself"
}

variable "embedding_vector_dimensions" {
  description = "Expected embedding vector dimensionality, surfaced as an output for the capabilities contract. Follows the active embedding model."
  type        = number
  default     = 1024
}

###############################################################################
# Object/blob storage (S3)
###############################################################################

variable "create_blob_storage" {
  description = "Provision an S3 bucket for exports, attachments, diagnostic bundles, and backups."
  type        = bool
  default     = true
}

###############################################################################
# Sealed plane — KMS (required only when secrets + TOTP are enabled)
#
# The open plane (memories + facts) never needs KMS. When the sealed plane is
# off, the KMS submodule provisions nothing. The deployment identity is granted
# envelope operations only — never ScheduleKeyDeletion or key-policy admin.
# See docs/key-hierarchy.md and docs/encryption-model.md.
###############################################################################

variable "sealed_plane_enabled" {
  description = "Enable the sealed plane (secrets + TOTP). When true, a KMS CMK is provisioned (or an existing CMK is referenced via existing_kms_key_arn)."
  type        = bool
  default     = false
}

variable "existing_kms_key_arn" {
  description = "BYOK: ARN of an existing KMS CMK to use for the sealed plane instead of provisioning one. Empty provisions a new CMK when sealed_plane_enabled is true."
  type        = string
  default     = ""
}

variable "kms_key_rotation_enabled" {
  description = "Enable annual rotation on a newly provisioned sealed-plane CMK."
  type        = bool
  default     = true
}

###############################################################################
# Workload identity (IRSA) — for the witself-server deployment
###############################################################################

variable "kubernetes_namespace" {
  description = "Kubernetes namespace the witself-server ServiceAccount lives in (used for the IRSA trust policy)."
  type        = string
  default     = "witself"
}

variable "service_account_name" {
  description = "Name of the witself-server Kubernetes ServiceAccount the IRSA role is bound to."
  type        = string
  default     = "witself-server"
}

###############################################################################
# Optional DNS / TLS
###############################################################################

variable "public_hostname" {
  description = "Optional public hostname for the deployment (Route 53 / ACM integration). Empty disables DNS/cert wiring."
  type        = string
  default     = ""
}

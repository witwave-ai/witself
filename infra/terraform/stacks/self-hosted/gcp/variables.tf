# Self-hosted GCP stack — input variables.
#
# SKELETON: AWS-first. Supply real values via a gitignored terraform.tfvars or
# -var flags. Never commit a real tfvars file.

variable "name" {
  description = "Name prefix for all provisioned resources."
  type        = string
  default     = "witself"
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

variable "sealed_plane_enabled" {
  description = "Enable the sealed plane (secrets + TOTP). When true, a Cloud KMS key is provisioned. Leave false for an open-plane-only deployment."
  type        = bool
  default     = false
}

# Self-hosted Azure stack — input variables.
#
# SKELETON: AWS-first. Supply real values via a gitignored terraform.tfvars or
# -var flags. Never commit a real tfvars file.

variable "name" {
  description = "Name prefix for all provisioned resources."
  type        = string
  default     = "witself"
}

variable "resource_group_name" {
  description = "Azure resource group to deploy into."
  type        = string
}

variable "location" {
  description = "Azure region."
  type        = string
  default     = "eastus"
}

variable "sealed_plane_enabled" {
  description = "Enable the sealed plane (secrets + TOTP). When true, an Azure Key Vault key is provisioned. Leave false for an open-plane-only deployment."
  type        = bool
  default     = false
}

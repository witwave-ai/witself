# Azure module — input variables.
#
# SKELETON: AWS-first. Azure is a planned target (AKS, Azure Database for
# PostgreSQL with pgvector, Azure Blob Storage, Azure Workload Identity, Azure
# Key Vault key for the sealed plane). See docs/terraform-infrastructure.md.

variable "name" {
  description = "Name prefix for all provisioned resources."
  type        = string
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
  description = "Enable the sealed plane (secrets + TOTP). When true, an Azure Key Vault key is provisioned for the envelope root. The open plane never needs KMS."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags applied to resources that support them."
  type        = map(string)
  default     = {}
}

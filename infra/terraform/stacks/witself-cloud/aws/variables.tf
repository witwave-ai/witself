# Witself Cloud AWS stack — input variables.
#
# Public shape only. Real production values (account IDs, hostnames, key ARNs,
# topology) are supplied through private environment overlays and a gitignored
# tfvars, never committed here.

variable "name" {
  description = "Name prefix for managed Witself Cloud resources."
  type        = string
  default     = "witself-cloud"
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-1"
}

variable "public_hostname" {
  description = "Public hostname for managed Witself Cloud. Supplied per environment; never a real value in the public repo."
  type        = string
  default     = ""
}

variable "existing_kms_key_arn" {
  description = "BYOK CMK ARN for the sealed plane. Managed Cloud keeps the key lifecycle in a private account; never a real ARN in the public repo."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Additional tags applied to all resources."
  type        = map(string)
  default     = {}
}

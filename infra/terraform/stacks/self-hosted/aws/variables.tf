# Self-hosted AWS stack — input variables.
#
# Supply real values via a local terraform.tfvars (gitignored) or -var flags.
# See terraform.tfvars.example. Never commit a real tfvars file.

variable "name" {
  description = "Name prefix for all provisioned resources (e.g. \"witself\")."
  type        = string
  default     = "witself"
}

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "us-east-1"
}

variable "sealed_plane_enabled" {
  description = "Enable the sealed plane (secrets + TOTP). When true, a KMS CMK is provisioned for the envelope root. Leave false for an open-plane-only deployment."
  type        = bool
  default     = false
}

variable "existing_kms_key_arn" {
  description = "BYOK: ARN of an existing CMK for the sealed plane instead of provisioning one. Empty provisions a new CMK when sealed_plane_enabled is true."
  type        = string
  default     = ""
}

variable "public_hostname" {
  description = "Optional public hostname for the deployment (Route 53 / ACM). Empty disables DNS/cert wiring."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Additional tags applied to all resources."
  type        = map(string)
  default     = {}
}

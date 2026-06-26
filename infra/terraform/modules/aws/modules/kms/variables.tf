# AWS KMS submodule — input variables.
#
# Provisions the sealed plane's customer managed key (CMK), the root of the
# envelope hierarchy (CMK -> per-realm KEK -> per-secret/field DEK). Required only
# when the sealed plane (secrets + TOTP) is enabled. See docs/key-hierarchy.md.

variable "name" {
  description = "Name prefix for the CMK alias and IAM policy."
  type        = string
}

variable "existing_kms_key_arn" {
  description = "BYOK: ARN of an existing CMK to use instead of provisioning one. Empty provisions a new CMK so operators can keep the key lifecycle outside the public stack."
  type        = string
  default     = ""
}

variable "key_rotation_enabled" {
  description = "Enable annual key rotation on a newly provisioned CMK."
  type        = bool
  default     = true
}

variable "deployment_role_arn" {
  description = "ARN of the witself-server deployment identity (IRSA role) granted envelope operations on the CMK. Scoped to this identity, not the account root, to limit blast radius."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags applied to the CMK."
  type        = map(string)
  default     = {}
}
